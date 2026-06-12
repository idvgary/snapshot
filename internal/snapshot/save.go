package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	runsOnConfig "github.com/runs-on/snapshot/internal/config"
)

const (
	defaultVolumeLifeDurationMinutes int32 = 20
)

func (s *AWSSnapshotter) CleanupVolume(ctx context.Context, mountPoint string) error {
	volumeInfo, err := s.loadVolumeInfo(mountPoint)
	if err != nil {
		return fmt.Errorf("failed to load volume info: %w", err)
	}
	if err := s.unmountAndDetachVolume(ctx, mountPoint, volumeInfo, s.config.WaitForCleanup); err != nil {
		return err
	}
	if s.config.WaitForCleanup {
		s.deleteVolume(ctx, volumeInfo.VolumeID)
	}
	return nil
}

func (s *AWSSnapshotter) CreateSnapshot(ctx context.Context, mountPoint string) (*CreateSnapshotOutput, error) {
	gitBranch := s.config.GithubRef
	s.logger.Info().Msgf("CreateSnapshot: Using git ref: %s, Instance ID: %s, MountPoint: %s", gitBranch, s.config.InstanceID, mountPoint)

	// Load volume info from JSON file
	volumeInfo, err := s.loadVolumeInfo(mountPoint)
	if err != nil {
		return nil, fmt.Errorf("failed to load volume info: %w", err)
	}

	saveDecision, err := s.shouldSaveSnapshot(ctx, mountPoint)
	if err != nil {
		return nil, err
	}
	if saveDecision.skip {
		s.logger.Info().Msgf("CreateSnapshot: Save decision: skip: %s", saveDecision.reason)
	} else {
		s.logger.Info().Msgf("CreateSnapshot: Save decision: %s", saveDecision.reason)
		if err := s.saveSourceMetadata(mountPoint, SourceMetadata{
			SHA:               saveDecision.headSHA,
			Ref:               s.config.GithubRef,
			Repository:        s.config.GithubRepository,
			Key:               s.config.Key,
			Version:           s.config.Version,
			SavePolicyName:    s.config.SavePolicyName,
			SavePolicyVersion: s.config.SavePolicyVersion,
			SavedAt:           time.Now(),
		}); err != nil {
			return nil, err
		}
	}

	if err := s.unmountAndDetachVolume(ctx, mountPoint, volumeInfo, !saveDecision.skip || s.config.WaitForCleanup); err != nil {
		return nil, err
	}

	if saveDecision.skip {
		if s.config.WaitForCleanup {
			s.deleteVolume(ctx, volumeInfo.VolumeID)
		}
		return &CreateSnapshotOutput{
			Skipped:       true,
			SkipReason:    saveDecision.reason,
			ChangedPaths:  saveDecision.changedPaths,
			SourceSHA:     saveDecision.headSHA,
			BaseSourceSHA: saveDecision.baseSHA,
		}, nil
	}

	// 3. Create new snapshot
	currentTime := time.Now()
	s.logger.Info().Msgf("CreateSnapshot: Creating snapshot '%s' from volume %s for branch %s...", s.config.SnapshotName, volumeInfo.VolumeID, s.config.GithubRef)
	metadata := SourceMetadata{
		SHA:               saveDecision.headSHA,
		Ref:               s.config.GithubRef,
		Repository:        s.config.GithubRepository,
		Key:               s.config.Key,
		Version:           s.config.Version,
		SavePolicyName:    s.config.SavePolicyName,
		SavePolicyVersion: s.config.SavePolicyVersion,
		SavedAt:           time.Now(),
	}
	snapshotTags := append(s.defaultTags(), []types.Tag{
		{Key: aws.String(nameTagKey), Value: aws.String(s.config.SnapshotName)},
	}...)
	snapshotTags = append(snapshotTags, s.sourceTags(metadata)...)
	createSnapshotOutput, err := s.ec2Client.CreateSnapshot(ctx, &ec2.CreateSnapshotInput{
		VolumeId: aws.String(volumeInfo.VolumeID),
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeSnapshot,
				Tags:         snapshotTags,
			},
		},
		Description: aws.String(fmt.Sprintf("Snapshot for branch %s taken at %s", s.config.GithubRef, currentTime.Format(time.RFC3339))),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create snapshot from volume %s: %w", volumeInfo.VolumeID, err)
	}
	newSnapshotID := *createSnapshotOutput.SnapshotId
	s.logger.Info().Msgf("CreateSnapshot: Snapshot %s creation initiated.", newSnapshotID)
	s.deleteVolume(ctx, volumeInfo.VolumeID)
	if err := s.pruneSnapshots(ctx, newSnapshotID); err != nil {
		s.logger.Warn().Msgf("CreateSnapshot: snapshot pruning failed: %v", err)
	}

	if volumeInfo.NewVolume {
		s.logger.Info().Msgf("CreateSnapshot: creating from a new volume, so waiting for initial snapshot completion. This may take a few minutes.")
	} else if s.config.WaitForCompletion {
		s.logger.Info().Msgf("CreateSnapshot: waiting for snapshot completion before returning.")
	} else {
		s.logger.Info().Msgf("CreateSnapshot: not waiting for snapshot completion, returning immediately.")
		return &CreateSnapshotOutput{SnapshotID: newSnapshotID, SaveReason: saveDecision.reason, ChangedPaths: saveDecision.changedPaths, SourceSHA: saveDecision.headSHA, BaseSourceSHA: saveDecision.baseSHA}, nil
	}

	s.logger.Info().Msgf("CreateSnapshot: Waiting for snapshot %s completion...", newSnapshotID)
	snapshotCompletedWaiter := ec2.NewSnapshotCompletedWaiter(s.ec2Client, defaultSnapshotCompletedWaiterOptions)
	if err := snapshotCompletedWaiter.Wait(ctx, &ec2.DescribeSnapshotsInput{SnapshotIds: []string{newSnapshotID}}, defaultSnapshotCompletedMaxWaitTime); err != nil {
		return nil, fmt.Errorf("snapshot %s did not complete in time: %w", newSnapshotID, err)
	}
	s.logger.Info().Msgf("CreateSnapshot: Snapshot %s completed.", newSnapshotID)

	return &CreateSnapshotOutput{SnapshotID: newSnapshotID, SaveReason: saveDecision.reason, ChangedPaths: saveDecision.changedPaths, SourceSHA: saveDecision.headSHA, BaseSourceSHA: saveDecision.baseSHA}, nil
}

func (s *AWSSnapshotter) unmountAndDetachVolume(ctx context.Context, mountPoint string, volumeInfo *VolumeInfo, waitForCleanup bool) error {
	if strings.HasPrefix(mountPoint, "/var/lib/docker") {
		s.logger.Info().Msgf("CreateSnapshot: Cleaning up useless files...")
		if _, err := s.runCommand(ctx, "sudo", "docker", "builder", "prune", "-f"); err != nil {
			s.logger.Warn().Msgf("Warning: failed to prune docker builder: %v", err)
		}

		s.logger.Info().Msgf("CreateSnapshot: Stopping docker service...")
		if _, err := s.runCommand(ctx, "sudo", "systemctl", "stop", "docker"); err != nil {
			s.logger.Warn().Msgf("Warning: failed to stop docker (may not be running or installed): %v", err)
		}
	}

	s.logger.Info().Msgf("CreateSnapshot: Unmounting %s (from device %s, volume %s)...", mountPoint, volumeInfo.DeviceName, volumeInfo.VolumeID)
	if _, err := s.runCommand(ctx, "sudo", "umount", mountPoint); err != nil {
		dfOutput, checkErr := s.runCommand(ctx, "df", mountPoint)
		if checkErr == nil && strings.Contains(string(dfOutput), mountPoint) { // If still mounted, then error
			return fmt.Errorf("failed to unmount %s: %w. Output: %s", mountPoint, err, string(dfOutput))
		}
		s.logger.Warn().Msgf("CreateSnapshot: Unmount of %s failed but it seems not mounted anymore: %v", mountPoint, err)
	} else {
		s.logger.Info().Msgf("CreateSnapshot: Successfully unmounted %s.", mountPoint)
	}

	// Update TTL tag on volume to extend until 10min from now
	_, err := s.ec2Client.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{volumeInfo.VolumeID},
		Tags: []types.Tag{
			{Key: aws.String(ttlTagKey), Value: aws.String(fmt.Sprintf("%d", time.Now().Add(10*time.Minute).Unix()))},
		},
	})
	if err != nil {
		s.logger.Warn().Msgf("Failed to update TTL tag on volume %s: %v", volumeInfo.VolumeID, err)
	}

	s.logger.Info().Msgf("CreateSnapshot: Detaching volume %s...", volumeInfo.VolumeID)
	_, err = s.ec2Client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId:   aws.String(volumeInfo.VolumeID),
		InstanceId: aws.String(s.config.InstanceID),
	})
	if err != nil {
		return fmt.Errorf("failed to initiate detach for volume %s: %w", volumeInfo.VolumeID, err)
	}
	if !waitForCleanup {
		s.logger.Info().Msgf("CreateSnapshot: detach initiated for volume %s; not waiting for cleanup.", volumeInfo.VolumeID)
		return nil
	}

	volumeDetachedWaiter := ec2.NewVolumeAvailableWaiter(s.ec2Client, defaultVolumeAvailableWaiterOptions) // Available state implies detached
	s.logger.Info().Msgf("CreateSnapshot: Waiting for volume %s to become available (detached)...", volumeInfo.VolumeID)
	if err := volumeDetachedWaiter.Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeInfo.VolumeID}}, defaultVolumeAvailableMaxWaitTime); err != nil {
		return fmt.Errorf("volume %s did not become available (detach) in time: %w", volumeInfo.VolumeID, err)
	}
	s.logger.Info().Msgf("CreateSnapshot: Volume %s is detached.", volumeInfo.VolumeID)
	return nil
}

type saveDecision struct {
	skip         bool
	reason       string
	changedPaths []string
	baseSHA      string
	headSHA      string
}

func (d *saveDecision) Skipped() bool {
	return d.skip
}

func (s *AWSSnapshotter) shouldSaveSnapshot(ctx context.Context, mountPoint string) (*saveDecision, error) {
	return ShouldSaveSnapshot(ctx, s.config, mountPoint, s.runCommand)
}

type commandRunner func(ctx context.Context, name string, arg ...string) ([]byte, error)

func ShouldSaveSnapshot(ctx context.Context, cfg *runsOnConfig.Config, mountPoint string, runCommand commandRunner) (*saveDecision, error) {
	headSHA := strings.TrimSpace(cfg.GitHead)
	if headSHA == "" {
		headSHA = os.Getenv("GITHUB_SHA")
	}
	decision := &saveDecision{headSHA: headSHA, reason: "save=true"}
	if cfg.SaveMarkerFile != "" {
		markerBytes, err := os.ReadFile(cfg.SaveMarkerFile)
		if err != nil {
			decision.skip = true
			decision.reason = fmt.Sprintf("save marker file missing: %v", err)
			return decision, nil
		}
		if strings.TrimSpace(string(markerBytes)) == "save=false" {
			decision.skip = true
			decision.reason = "save marker requested save=false"
			return decision, nil
		}
	}
	if cfg.ForceSave {
		decision.reason = "force-save=true"
		return decision, nil
	}

	if cfg.SaveMode != "auto" {
		return decision, nil
	}

	metadata, err := readSourceMetadata(mountPoint)
	if err != nil {
		if !cfg.SaveOnEmpty {
			decision.skip = true
			decision.reason = fmt.Sprintf("save-auto: missing restored source metadata and save-on-empty=false: %v", err)
			return decision, nil
		}
		decision.reason = fmt.Sprintf("save-auto: missing restored source metadata: %v", err)
		return decision, nil
	}
	decision.baseSHA = metadata.SHA
	if metadata.SHA == "" || headSHA == "" {
		decision.reason = "save-auto: missing base or head source sha"
		return decision, nil
	}
	if metadata.SHA == headSHA {
		decision.skip = true
		decision.reason = "save-auto: restored source sha already matches current sha"
		return decision, nil
	}

	if cfg.SaveIf != "git-paths-changed" {
		decision.reason = fmt.Sprintf("save-auto: unsupported or always save-if policy %q", cfg.SaveIf)
		return decision, nil
	}
	if len(cfg.GitPaths) == 0 {
		decision.reason = "save-auto: no git paths configured"
		return decision, nil
	}

	repoPath := cfg.GitRepository
	if repoPath == "" {
		repoPath = filepath.Join(mountPoint, "workspace")
	}
	changedPaths, err := changedGitPaths(ctx, runCommand, repoPath, metadata.SHA, headSHA, cfg.GitPaths)
	if err != nil {
		decision.reason = fmt.Sprintf("save-auto: failed to evaluate git path changes: %v", err)
		return decision, nil
	}
	decision.changedPaths = changedPaths
	if len(changedPaths) == 0 {
		decision.skip = true
		decision.reason = "save-auto: no relevant git path changes"
		return decision, nil
	}
	decision.reason = fmt.Sprintf("save-auto: %d relevant git path changes", len(changedPaths))
	return decision, nil
}

func changedGitPaths(ctx context.Context, runCommand commandRunner, repoPath string, baseSHA string, headSHA string, pathspecs []string) ([]string, error) {
	args := []string{"-C", repoPath, "diff", "--name-only", baseSHA, headSHA, "--"}
	args = append(args, pathspecs...)
	output, err := runCommand(ctx, "git", args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func (s *AWSSnapshotter) pruneSnapshots(ctx context.Context, currentSnapshotID string) error {
	if s.config.KeepLastSnapshots <= 0 {
		return nil
	}
	filters := []types.Filter{
		{Name: aws.String("status"), Values: []string{string(types.SnapshotStateCompleted), string(types.SnapshotStatePending)}},
	}
	for _, tag := range s.defaultTags() {
		filters = append(filters, types.Filter{Name: aws.String(fmt.Sprintf("tag:%s", *tag.Key)), Values: []string{*tag.Value}})
	}
	paginator := ec2.NewDescribeSnapshotsPaginator(s.ec2Client, &ec2.DescribeSnapshotsInput{
		Filters:  filters,
		OwnerIds: []string{"self"},
	})
	var snapshots []types.Snapshot
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return err
		}
		snapshots = append(snapshots, page.Snapshots...)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].StartTime == nil {
			return false
		}
		if snapshots[j].StartTime == nil {
			return true
		}
		return snapshots[i].StartTime.After(*snapshots[j].StartTime)
	})
	keep := int(s.config.KeepLastSnapshots)
	if keep < 1 {
		keep = 1
	}
	for i, snap := range snapshots {
		if snap.SnapshotId == nil {
			continue
		}
		if *snap.SnapshotId == currentSnapshotID {
			continue
		}
		if i < keep {
			continue
		}
		s.logger.Info().Msgf("Pruning old snapshot %s for key %s", *snap.SnapshotId, s.config.Key)
		_, err := s.ec2Client.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: snap.SnapshotId})
		if err != nil {
			s.logger.Warn().Msgf("Failed to delete old snapshot %s: %v", *snap.SnapshotId, err)
		}
	}
	return nil
}
