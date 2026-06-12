package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/runs-on/snapshot/internal/utils"
)

type snapshotSearchCandidate struct {
	branch string
	key    string
	source string
}

// RestoreSnapshot finds the latest snapshot for the current git branch,
// creates a volume from it (or a new volume if no snapshot exists),
// attaches it to the instance, and mounts it to the specified mountPoint.
func (s *AWSSnapshotter) RestoreSnapshot(ctx context.Context, mountPoint string) (output *RestoreSnapshotOutput, retErr error) {
	gitBranch := s.config.GithubRef
	s.logger.Info().Msgf("RestoreSnapshot: Using git ref: %s and key: %s", gitBranch, s.config.Key)

	var err error

	var newVolume *types.Volume
	var volumeIsNewAndUnformatted bool
	var volumeNeedsResize bool
	var latestSnapshot *types.Snapshot
	restoredFrom := "empty"
	restoredBranch := ""

	for _, candidate := range s.snapshotSearchCandidates() {
		candidateSnapshot, err := s.findLatestSnapshot(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if candidateSnapshot == nil {
			continue
		}
		latestSnapshot = candidateSnapshot
		restoredFrom = candidate.source
		restoredBranch = candidate.branch
		break
	}

	if latestSnapshot == nil {
		s.logger.Info().Msgf("RestoreSnapshot: No existing snapshot found. A new volume will be created.")
	}

	commonVolumeTags := append(s.defaultTags(), []types.Tag{
		{Key: aws.String(nameTagKey), Value: aws.String(s.config.VolumeName)},
		{Key: aws.String(ttlTagKey), Value: aws.String(fmt.Sprintf("%d", time.Now().Add(time.Duration(defaultVolumeLifeDurationMinutes)*time.Minute).Unix()))},
	}...)

	s.logger.Info().Msgf("RestoreSnapshot: common volume tags: %s", utils.PrettyPrint(commonVolumeTags))

	if latestSnapshot != nil && latestSnapshot.VolumeSize != nil {
		// 2. Create Volume from Snapshot
		s.logger.Info().Msgf("RestoreSnapshot: Creating volume from snapshot %s", *latestSnapshot.SnapshotId)
		createVolumeInput := &ec2.CreateVolumeInput{
			ClientToken:      s.createVolumeClientToken("snapshot:" + snapshotID(latestSnapshot)),
			SnapshotId:       latestSnapshot.SnapshotId,
			AvailabilityZone: aws.String(s.config.Az),
			VolumeType:       s.config.VolumeType,
			TagSpecifications: []types.TagSpecification{
				{ResourceType: types.ResourceTypeVolume, Tags: commonVolumeTags},
			},
		}
		if volumeTypeSupportsIops(s.config.VolumeType) {
			createVolumeInput.Iops = aws.Int32(s.config.VolumeIops)
		}
		// Throughput is only supported for gp3 volumes
		if s.config.VolumeType == types.VolumeTypeGp3 {
			createVolumeInput.Throughput = aws.Int32(s.config.VolumeThroughput)
		}
		if s.config.VolumeInitializationRate > 0 {
			createVolumeInput.VolumeInitializationRate = aws.Int32(s.config.VolumeInitializationRate)
		}
		if *latestSnapshot.VolumeSize < s.config.VolumeSize {
			createVolumeInput.Size = aws.Int32(s.config.VolumeSize)
			volumeNeedsResize = true
		}
		createVolumeOutput, err := s.ec2Client.CreateVolume(ctx, createVolumeInput)
		if err != nil {
			return nil, fmt.Errorf("failed to create volume from snapshot %s: %w", *latestSnapshot.SnapshotId, err)
		}
		newVolume = &types.Volume{VolumeId: createVolumeOutput.VolumeId}
		volumeIsNewAndUnformatted = false // Volume from snapshot is already formatted
		s.logger.Info().Msgf("RestoreSnapshot: Created volume %s from snapshot %s", *newVolume.VolumeId, *latestSnapshot.SnapshotId)
	} else {
		// 3. No snapshot found, create a new volume
		s.logger.Info().Msgf("RestoreSnapshot: Creating a new blank volume")
		createVolumeInput := &ec2.CreateVolumeInput{
			ClientToken:      s.createVolumeClientToken("blank"),
			AvailabilityZone: aws.String(s.config.Az),
			VolumeType:       s.config.VolumeType,
			Size:             aws.Int32(s.config.VolumeSize),
			TagSpecifications: []types.TagSpecification{
				{ResourceType: types.ResourceTypeVolume, Tags: commonVolumeTags},
			},
		}
		if volumeTypeSupportsIops(s.config.VolumeType) {
			createVolumeInput.Iops = aws.Int32(s.config.VolumeIops)
		}
		// Throughput is only supported for gp3 volumes
		if s.config.VolumeType == types.VolumeTypeGp3 {
			createVolumeInput.Throughput = aws.Int32(s.config.VolumeThroughput)
		}
		createVolumeOutput, err := s.ec2Client.CreateVolume(ctx, createVolumeInput)
		if err != nil {
			return nil, fmt.Errorf("failed to create new volume: %w", err)
		}
		newVolume = &types.Volume{VolumeId: createVolumeOutput.VolumeId}
		volumeIsNewAndUnformatted = true // New volume needs formatting
		s.logger.Info().Msgf("RestoreSnapshot: Created new blank volume %s", *newVolume.VolumeId)
	}

	defer func() {
		if retErr != nil && newVolume != nil && newVolume.VolumeId != nil {
			s.logger.Error().Msgf("RestoreSnapshot: Error: %v", retErr)
			if _, unmountErr := s.runCommand(ctx, "sudo", "umount", mountPoint); unmountErr != nil {
				s.logger.Warn().Msgf("RestoreSnapshot: cleanup unmount of %s failed: %v", mountPoint, unmountErr)
			}
			_, detachErr := s.ec2Client.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: newVolume.VolumeId, InstanceId: aws.String(s.config.InstanceID)})
			if detachErr != nil {
				s.logger.Warn().Msgf("RestoreSnapshot: cleanup detach of volume %s failed: %v", *newVolume.VolumeId, detachErr)
			} else {
				volumeAvailableWaiter := ec2.NewVolumeAvailableWaiter(s.ec2Client, defaultVolumeAvailableWaiterOptions)
				if waitErr := volumeAvailableWaiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{*newVolume.VolumeId}}, defaultVolumeAvailableMaxWaitTime); waitErr != nil {
					s.logger.Warn().Msgf("RestoreSnapshot: cleanup wait for detach of volume %s failed: %v", *newVolume.VolumeId, waitErr)
				}
			}
			if newVolume != nil {
				s.logger.Info().Msgf("RestoreSnapshot: Deleting volume %s", *newVolume.VolumeId)
				_, deleteErr := s.ec2Client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: newVolume.VolumeId})
				if deleteErr != nil {
					s.logger.Error().Msgf("RestoreSnapshot: Error deleting volume %s: %v", *newVolume.VolumeId, deleteErr)
				}
			}
		}
	}()

	// 4. Wait for volume to be 'available'
	s.logger.Info().Msgf("RestoreSnapshot: Waiting for volume %s to become available...", *newVolume.VolumeId)
	volumeAvailableWaiter := ec2.NewVolumeAvailableWaiter(s.ec2Client, defaultVolumeAvailableWaiterOptions)
	if err := volumeAvailableWaiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{*newVolume.VolumeId}}, defaultVolumeAvailableMaxWaitTime); err != nil {
		return nil, fmt.Errorf("volume %s did not become available in time: %w", *newVolume.VolumeId, err)
	}
	s.logger.Info().Msgf("RestoreSnapshot: Volume %s is available.", *newVolume.VolumeId)

	// 5. Attach Volume
	deviceName, err := s.nextAttachmentDeviceName(ctx)
	if err != nil {
		return nil, err
	}
	s.logger.Info().Msgf("RestoreSnapshot: Attaching volume %s to instance %s as %s", *newVolume.VolumeId, s.config.InstanceID, deviceName)
	attachOutput, err := s.ec2Client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		Device:     aws.String(deviceName),
		InstanceId: aws.String(s.config.InstanceID),
		VolumeId:   newVolume.VolumeId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to attach volume %s to instance %s: %w", *newVolume.VolumeId, s.config.InstanceID, err)
	}
	actualDeviceName := *attachOutput.Device
	s.logger.Info().Msgf("RestoreSnapshot: Volume %s attach initiated, device hint: %s. Waiting for attachment...", *newVolume.VolumeId, actualDeviceName)

	volumeInUseWaiter := ec2.NewVolumeInUseWaiter(s.ec2Client, defaultVolumeInUseWaiterOptions)
	err = volumeInUseWaiter.Wait(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{*newVolume.VolumeId},
		Filters: []types.Filter{
			{
				Name:   aws.String("attachment.status"),
				Values: []string{"attached"},
			},
		},
	}, defaultVolumeInUseMaxWaitTime)
	if err != nil {
		return nil, fmt.Errorf("volume %s did not attach successfully and current state unknown: %w", *newVolume.VolumeId, err)
	}
	// Fetch volume details again to confirm device name, as the attachOutput.Device might be a suggestion
	// and the waiter confirms attachment, not necessarily the final device name if it changed.
	descVolOutput, descErr := s.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{*newVolume.VolumeId}})
	if descErr == nil && len(descVolOutput.Volumes) > 0 && len(descVolOutput.Volumes[0].Attachments) > 0 {
		s.logger.Info().Msgf("RestoreSnapshot: Volume %s attachments: %v", *newVolume.VolumeId, descVolOutput.Volumes[0].Attachments)
		actualDeviceName = *descVolOutput.Volumes[0].Attachments[0].Device
	} else {
		return nil, fmt.Errorf("volume %s did not attach successfully and current state unknown: %w", *newVolume.VolumeId, err)
	}
	s.logger.Info().Msgf("RestoreSnapshot: Volume %s attached as %s.", *newVolume.VolumeId, actualDeviceName)
	resolvedDeviceName, err := s.resolveDeviceName(ctx, *newVolume.VolumeId, actualDeviceName)
	if err != nil {
		return nil, err
	}
	actualDeviceName = resolvedDeviceName

	if strings.HasPrefix(mountPoint, "/var/lib/docker") {
		// 6. Mounting & Docker
		s.logger.Info().Msgf("RestoreSnapshot: Stopping docker service...")
		if _, err := s.runCommand(ctx, "sudo", "systemctl", "stop", "docker"); err != nil {
			s.logger.Warn().Msgf("RestoreSnapshot: failed to stop docker (may not be running or installed): %v", err)

		}
	}

	s.logger.Info().Msgf("RestoreSnapshot: Attempting to unmount %s (defensive)", mountPoint)
	if _, err := s.runCommand(ctx, "sudo", "umount", mountPoint); err != nil {
		s.logger.Warn().Msgf("RestoreSnapshot: Defensive unmount of %s failed (likely not mounted): %v", mountPoint, err)
	}

	s.logger.Info().Msgf("RestoreSnapshot: Actual device name: %s", actualDeviceName)

	// Save volume info to JSON file
	volumeInfo := &VolumeInfo{
		VolumeID:   *newVolume.VolumeId,
		DeviceName: actualDeviceName,
		MountPoint: mountPoint,
		NewVolume:  volumeIsNewAndUnformatted,
	}
	if err := s.saveVolumeInfo(volumeInfo); err != nil {
		s.logger.Warn().Msgf("RestoreSnapshot: Failed to save volume info: %v", err)
	}

	if volumeIsNewAndUnformatted {
		s.logger.Info().Msgf("RestoreSnapshot: Formatting new volume %s (%s) with ext4...", *newVolume.VolumeId, actualDeviceName)
		if _, err := s.runCommand(ctx, "sudo", "mkfs.ext4", "-F", actualDeviceName); err != nil { // -F to force if already formatted by mistake or small
			return nil, fmt.Errorf("failed to format device %s: %w", actualDeviceName, err)
		}
		s.logger.Info().Msgf("RestoreSnapshot: Device %s formatted.", actualDeviceName)
	}

	s.logger.Info().Msgf("RestoreSnapshot: Creating mount point %s if it doesn't exist...", mountPoint)
	if _, err := s.runCommand(ctx, "sudo", "mkdir", "-p", mountPoint); err != nil {
		return nil, fmt.Errorf("failed to create mount point %s: %w", mountPoint, err)
	}

	s.logger.Info().Msgf("RestoreSnapshot: Mounting %s to %s...", actualDeviceName, mountPoint)
	if _, err := s.runCommand(ctx, "sudo", "mount", actualDeviceName, mountPoint); err != nil {
		return nil, fmt.Errorf("failed to mount %s to %s: %w", actualDeviceName, mountPoint, err)
	}
	s.logger.Info().Msgf("RestoreSnapshot: Device %s mounted to %s.", actualDeviceName, mountPoint)
	if volumeNeedsResize {
		s.logger.Info().Msgf("RestoreSnapshot: Resizing ext4 filesystem on %s after restoring smaller snapshot into larger volume...", actualDeviceName)
		if _, err := s.runCommand(ctx, "sudo", "resize2fs", actualDeviceName); err != nil {
			return nil, fmt.Errorf("failed to resize filesystem on %s: %w", actualDeviceName, err)
		}
	}

	if strings.HasPrefix(mountPoint, "/var/lib/docker") {
		s.logger.Info().Msgf("RestoreSnapshot: Starting docker service...")
		if _, err := s.runCommand(ctx, "sudo", "systemctl", "start", "docker"); err != nil {
			return nil, fmt.Errorf("failed to start docker after mounting: %w", err)
		}
		s.logger.Info().Msgf("RestoreSnapshot: Docker service started.")

		s.logger.Info().Msgf("RestoreSnapshot: Displaying docker disk usage...")
		if _, err := s.runCommand(ctx, "sudo", "docker", "system", "info"); err != nil {
			s.logger.Warn().Msgf("RestoreSnapshot: failed to display docker info: %v. Docker snapshot may not be working so unmounting docker folder.", err)
			// Try to unmount docker folder on error
			if _, err := s.runCommand(ctx, "sudo", "umount", mountPoint); err != nil {
				s.logger.Warn().Msgf("RestoreSnapshot: failed to unmount docker folder: %v", err)
			}
			return nil, fmt.Errorf("failed to display docker disk usage: %w", err)
		}
		s.logger.Info().Msgf("RestoreSnapshot: Docker disk usage displayed.")
	}

	restoredSourceSHA := ""
	restoredSourceRef := ""
	if metadata, err := s.loadSourceMetadata(mountPoint); err == nil {
		restoredSourceSHA = metadata.SHA
		restoredSourceRef = metadata.Ref
		s.logger.Info().Msgf("RestoreSnapshot: restored source metadata sha=%s ref=%s", restoredSourceSHA, restoredSourceRef)
	} else if latestSnapshot != nil {
		s.logger.Info().Msgf("RestoreSnapshot: no source metadata found in restored snapshot: %v", err)
	}

	return &RestoreSnapshotOutput{
		VolumeID:           *newVolume.VolumeId,
		Restored:           latestSnapshot != nil,
		RestoredFrom:       restoredFrom,
		RestoredBranch:     restoredBranch,
		RestoredSnapshotID: snapshotID(latestSnapshot),
		RestoredSourceSHA:  restoredSourceSHA,
		RestoredSourceRef:  restoredSourceRef,
	}, nil
}

func (s *AWSSnapshotter) nextAttachmentDeviceName(ctx context.Context) (string, error) {
	used := make(map[string]bool)
	descOutput, err := s.ec2Client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []string{s.config.InstanceID}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed to describe current EBS attachments for instance %s: %w", s.config.InstanceID, err)
	}
	for _, volume := range descOutput.Volumes {
		for _, attachment := range volume.Attachments {
			if attachment.Device != nil {
				used[*attachment.Device] = true
			}
		}
	}
	return nextAvailableAttachmentDeviceName(used)
}

func nextAvailableAttachmentDeviceName(used map[string]bool) (string, error) {
	for _, candidate := range attachmentDeviceCandidates {
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free EBS attachment device names available")
}

func (s *AWSSnapshotter) snapshotSearchCandidates() []snapshotSearchCandidate {
	candidates := []snapshotSearchCandidate{{branch: s.config.GithubRef, key: s.config.Key, source: "branch"}}
	for _, key := range s.config.RestoreKeys {
		candidates = append(candidates, snapshotSearchCandidate{branch: s.config.GithubRef, key: key, source: "restore-key"})
	}

	defaultBranch := s.config.RunnerConfig.DefaultBranch
	if s.config.DefaultBranchFallback && defaultBranch != "" && defaultBranch != s.config.GithubRef {
		candidates = append(candidates, snapshotSearchCandidate{branch: defaultBranch, key: s.config.Key, source: "default-branch"})
		for _, key := range s.config.RestoreKeys {
			candidates = append(candidates, snapshotSearchCandidate{branch: defaultBranch, key: key, source: "default-branch-restore-key"})
		}
	}

	seen := make(map[string]bool, len(candidates))
	uniqueCandidates := make([]snapshotSearchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		fingerprint := candidate.branch + "\x00" + candidate.key
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		uniqueCandidates = append(uniqueCandidates, candidate)
	}
	return uniqueCandidates
}

func (s *AWSSnapshotter) findLatestSnapshot(ctx context.Context, candidate snapshotSearchCandidate) (*types.Snapshot, error) {
	filters := []types.Filter{
		{Name: aws.String("status"), Values: []string{string(types.SnapshotStateCompleted)}},
	}
	for _, tag := range s.identityTags(candidate.branch, candidate.key) {
		filters = append(filters, types.Filter{Name: aws.String(fmt.Sprintf("tag:%s", *tag.Key)), Values: []string{*tag.Value}})
	}

	s.logger.Info().Msgf("RestoreSnapshot: Searching for latest snapshot source=%s branch=%s key=%s filters=%s", candidate.source, candidate.branch, candidate.key, utils.PrettyPrint(filters))
	paginator := ec2.NewDescribeSnapshotsPaginator(s.ec2Client, &ec2.DescribeSnapshotsInput{
		Filters:  filters,
		OwnerIds: []string{"self"},
	})
	var latestSnapshot *types.Snapshot
	for paginator.HasMorePages() {
		snapshotsOutput, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to describe snapshots for branch %s key %s: %w", candidate.branch, candidate.key, err)
		}
		for _, snap := range snapshotsOutput.Snapshots {
			snapCopy := snap
			if latestSnapshot == nil || (snapCopy.StartTime != nil && (latestSnapshot.StartTime == nil || snapCopy.StartTime.After(*latestSnapshot.StartTime))) {
				latestSnapshot = &snapCopy
			}
		}
	}
	if latestSnapshot == nil {
		return nil, nil
	}
	s.logger.Info().Msgf("RestoreSnapshot: Found latest snapshot %s source=%s branch=%s key=%s", *latestSnapshot.SnapshotId, candidate.source, candidate.branch, candidate.key)
	return latestSnapshot, nil
}

type lsblkDevice struct {
	Path   string `json:"path"`
	Serial string `json:"serial"`
	Model  string `json:"model"`
}

type lsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

func (s *AWSSnapshotter) resolveDeviceName(ctx context.Context, volumeID string, hintedDevice string) (string, error) {
	deadline := time.Now().Add(defaultDeviceResolveMaxWaitTime)
	for {
		deviceName, err := s.findDeviceName(ctx, volumeID, hintedDevice)
		if err == nil {
			return deviceName, nil
		}
		if time.Now().After(deadline) {
			return "", err
		}
		s.logger.Info().Msgf("RestoreSnapshot: waiting for block device for EBS volume %s: %v", volumeID, err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (s *AWSSnapshotter) findDeviceName(ctx context.Context, volumeID string, hintedDevice string) (string, error) {
	volumeSerial := strings.ReplaceAll(volumeID, "-", "")
	if output, err := s.runCommand(ctx, "lsblk", "-J", "-d", "-o", "PATH,SERIAL,MODEL"); err == nil {
		var parsed lsblkOutput
		if err := json.Unmarshal(output, &parsed); err == nil {
			for _, device := range parsed.Blockdevices {
				if strings.EqualFold(strings.ReplaceAll(device.Serial, "-", ""), volumeSerial) {
					return device.Path, nil
				}
			}
		} else {
			s.logger.Warn().Msgf("RestoreSnapshot: failed to parse lsblk JSON output: %v", err)
		}
	} else {
		s.logger.Warn().Msgf("RestoreSnapshot: lsblk serial lookup failed: %v", err)
	}

	byIDMatches, _ := filepath.Glob(filepath.Join("/dev/disk/by-id", "*"+volumeSerial+"*"))
	if len(byIDMatches) == 1 {
		resolvedPath, err := filepath.EvalSymlinks(byIDMatches[0])
		if err == nil {
			return resolvedPath, nil
		}
	}

	if hintedDevice != "" {
		if _, err := os.Stat(hintedDevice); err == nil {
			s.logger.Warn().Msgf("RestoreSnapshot: falling back to hinted device %s for volume %s", hintedDevice, volumeID)
			return hintedDevice, nil
		}
	}
	return "", fmt.Errorf("failed to resolve block device for EBS volume %s", volumeID)
}

func snapshotID(snapshot *types.Snapshot) string {
	if snapshot == nil || snapshot.SnapshotId == nil {
		return ""
	}
	return *snapshot.SnapshotId
}

func volumeTypeSupportsIops(volumeType types.VolumeType) bool {
	switch volumeType {
	case types.VolumeTypeGp3, types.VolumeTypeIo1, types.VolumeTypeIo2:
		return true
	default:
		return false
	}
}
