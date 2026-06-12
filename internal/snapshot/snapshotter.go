package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/rs/zerolog"
	runsOnConfig "github.com/runs-on/snapshot/internal/config"
	"github.com/runs-on/snapshot/internal/utils"
)

const (
	// Tags used for resource identification
	snapshotTagKeyArch       = "runs-on-snapshot-arch"
	snapshotTagKeyPlatform   = "runs-on-snapshot-platform"
	snapshotTagKeyBranch     = "runs-on-snapshot-branch"
	snapshotTagKeyRepository = "runs-on-snapshot-repository"
	repoFullNameTagKey       = "runs-on-repo-full-name"
	snapshotTagKeyKey        = "runs-on-snapshot-key"
	snapshotTagKeyKeyHash    = "runs-on-snapshot-key-hash"
	snapshotTagKeyPathHash   = "runs-on-snapshot-path-hash"
	snapshotTagKeySourceSHA  = "runs-on-snapshot-source-sha"
	snapshotTagKeySourceRef  = "runs-on-snapshot-source-ref"
	snapshotTagKeyPolicy     = "runs-on-snapshot-save-policy"
	snapshotTagKeyPolicyVer  = "runs-on-snapshot-save-policy-version"
	snapshotTagKeyVersion    = "runs-on-snapshot-version"
	nameTagKey               = "Name"
	ttlTagKey                = "runs-on-delete-after"

	defaultVolumeInUseMaxWaitTime       = 5 * time.Minute
	defaultVolumeAvailableMaxWaitTime   = 5 * time.Minute
	defaultSnapshotCompletedMaxWaitTime = 10 * time.Minute
	defaultDeviceResolveMaxWaitTime     = 30 * time.Second
)

var attachmentDeviceCandidates = []string{
	"/dev/sdf",
	"/dev/sdg",
	"/dev/sdh",
	"/dev/sdi",
	"/dev/sdj",
	"/dev/sdk",
	"/dev/sdl",
	"/dev/sdm",
	"/dev/sdn",
	"/dev/sdo",
	"/dev/sdp",
}

var defaultSnapshotCompletedWaiterOptions = func(o *ec2.SnapshotCompletedWaiterOptions) {
	o.MaxDelay = 3 * time.Second
	o.MinDelay = 3 * time.Second
}

var defaultVolumeInUseWaiterOptions = func(o *ec2.VolumeInUseWaiterOptions) {
	o.MaxDelay = 3 * time.Second
	o.MinDelay = 3 * time.Second
}

var defaultVolumeAvailableWaiterOptions = func(o *ec2.VolumeAvailableWaiterOptions) {
	o.MaxDelay = 3 * time.Second
	o.MinDelay = 3 * time.Second
}

// AWSSnapshotter provides methods to manage EBS snapshots and volumes.
type AWSSnapshotter struct {
	logger    *zerolog.Logger
	config    *runsOnConfig.Config
	ec2Client *ec2.Client
}

// RestoreSnapshotOutput holds the results of RestoreSnapshot.
type RestoreSnapshotOutput struct {
	VolumeID           string
	Restored           bool
	RestoredFrom       string
	RestoredBranch     string
	RestoredSnapshotID string
	RestoredSourceSHA  string
	RestoredSourceRef  string
}

// CreateSnapshotOutput holds the results of CreateSnapshot.
type CreateSnapshotOutput struct {
	SnapshotID    string
	Skipped       bool
	SkipReason    string
	SaveReason    string
	ChangedPaths  []string
	SourceSHA     string
	BaseSourceSHA string
}

// VolumeInfo stores information about the mounted volume
type VolumeInfo struct {
	VolumeID   string `json:"volume_id"`
	DeviceName string `json:"device_name"`
	MountPoint string `json:"mount_point"`
	KeyHash    string `json:"key_hash"`
	PathHash   string `json:"path_hash"`
	NewVolume  bool   `json:"new_volume,omitempty"`
}

// NewAWSSnapshotter creates a new AWSSnapshotter instance.
// It initializes the AWS SDK configuration and fetches EC2 instance metadata.
func NewAWSSnapshotter(ctx context.Context, logger *zerolog.Logger, cfg *runsOnConfig.Config) (*AWSSnapshotter, error) {
	awsConfig, err := utils.GetAWSClientFromEC2IMDS(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS SDK config: %w", err)
	}

	if cfg.InstanceID == "" {
		return nil, fmt.Errorf("instanceID is required")
	}

	if cfg.Az == "" {
		return nil, fmt.Errorf("az is required")
	}

	if cfg.GithubRepository == "" {
		return nil, fmt.Errorf("githubRepository is required")
	}

	if cfg.GithubRef == "" {
		return nil, fmt.Errorf("githubRef is required")
	}

	if cfg.CustomTags == nil {
		cfg.CustomTags = []runsOnConfig.Tag{}
	}

	sanitizedGithubRef := sanitizeNamePart(cfg.GithubRef, 32)
	sanitizedKey := sanitizeNamePart(cfg.Key, 48)

	currentTime := time.Now()
	if cfg.SnapshotName == "" {
		cfg.SnapshotName = fmt.Sprintf("runs-on-snapshot-%s-%s-%s", sanitizedGithubRef, sanitizedKey, currentTime.Format("20060102-150405"))
	}

	if cfg.VolumeName == "" {
		cfg.VolumeName = fmt.Sprintf("runs-on-volume-%s-%s-%s", sanitizedGithubRef, sanitizedKey, currentTime.Format("20060102-150405"))
	}

	return &AWSSnapshotter{
		logger:    logger,
		config:    cfg,
		ec2Client: ec2.NewFromConfig(*awsConfig),
	}, nil
}

func (s *AWSSnapshotter) arch() string {
	return runtime.GOARCH
}

func (s *AWSSnapshotter) platform() string {
	return runtime.GOOS
}

func (s *AWSSnapshotter) defaultTags() []types.Tag {
	return s.identityTags(s.config.GithubRef, s.config.Key)
}

func (s *AWSSnapshotter) identityTags(branch string, key string) []types.Tag {
	tags := []types.Tag{
		{Key: aws.String(snapshotTagKeyVersion), Value: aws.String(s.config.Version)},
		{Key: aws.String(snapshotTagKeyRepository), Value: aws.String(s.config.GithubRepository)},
		{Key: aws.String(repoFullNameTagKey), Value: aws.String(s.config.GithubRepository)},
		{Key: aws.String(snapshotTagKeyBranch), Value: aws.String(branch)},
		{Key: aws.String(snapshotTagKeyKey), Value: aws.String(truncateTagValue(key))},
		{Key: aws.String(snapshotTagKeyKeyHash), Value: aws.String(hashValue(key))},
		{Key: aws.String(snapshotTagKeyPathHash), Value: aws.String(hashValue(s.config.Path))},
		{Key: aws.String(snapshotTagKeyArch), Value: aws.String(s.arch())},
		{Key: aws.String(snapshotTagKeyPlatform), Value: aws.String(s.platform())},
	}
	for _, tag := range s.config.CustomTags {
		tags = append(tags, types.Tag{Key: aws.String(tag.Key), Value: aws.String(tag.Value)})
	}
	return tags
}

func (s *AWSSnapshotter) sourceTags(metadata SourceMetadata) []types.Tag {
	tags := []types.Tag{}
	if metadata.SHA != "" {
		tags = append(tags, types.Tag{Key: aws.String(snapshotTagKeySourceSHA), Value: aws.String(truncateTagValue(metadata.SHA))})
	}
	if metadata.Ref != "" {
		tags = append(tags, types.Tag{Key: aws.String(snapshotTagKeySourceRef), Value: aws.String(truncateTagValue(metadata.Ref))})
	}
	if metadata.SavePolicyName != "" {
		tags = append(tags, types.Tag{Key: aws.String(snapshotTagKeyPolicy), Value: aws.String(truncateTagValue(metadata.SavePolicyName))})
	}
	if metadata.SavePolicyVersion != "" {
		tags = append(tags, types.Tag{Key: aws.String(snapshotTagKeyPolicyVer), Value: aws.String(truncateTagValue(metadata.SavePolicyVersion))})
	}
	if s.config.RetentionDays > 0 {
		tags = append(tags, types.Tag{Key: aws.String(ttlTagKey), Value: aws.String(fmt.Sprintf("%d", time.Now().Add(time.Duration(s.config.RetentionDays)*24*time.Hour).Unix()))})
	}
	return tags
}

// saveVolumeInfo writes volume information to a JSON file
func (s *AWSSnapshotter) saveVolumeInfo(volumeInfo *VolumeInfo) error {
	volumeInfo.KeyHash = hashValue(s.config.Key)
	volumeInfo.PathHash = hashValue(volumeInfo.MountPoint)
	infoPath := s.getVolumeInfoPath(volumeInfo.MountPoint)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(infoPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory for volume info: %w", err)
	}

	data, err := json.MarshalIndent(volumeInfo, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal volume info: %w", err)
	}

	if err := os.WriteFile(infoPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write volume info file: %w", err)
	}

	return nil
}

// loadVolumeInfo reads volume information from a JSON file
func (s *AWSSnapshotter) loadVolumeInfo(mountPoint string) (*VolumeInfo, error) {
	infoPath := s.getVolumeInfoPath(mountPoint)
	data, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read volume info file: %w", err)
	}

	var volumeInfo VolumeInfo
	if err := json.Unmarshal(data, &volumeInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal volume info: %w", err)
	}

	return &volumeInfo, nil
}

type SourceMetadata struct {
	SHA               string    `json:"source_sha"`
	Ref               string    `json:"source_ref"`
	Repository        string    `json:"repository,omitempty"`
	Key               string    `json:"key,omitempty"`
	Version           string    `json:"version,omitempty"`
	SavePolicyName    string    `json:"save_policy_name,omitempty"`
	SavePolicyVersion string    `json:"save_policy_version,omitempty"`
	SavedAt           time.Time `json:"saved_at,omitempty"`
}

func (s *AWSSnapshotter) loadSourceMetadata(mountPoint string) (*SourceMetadata, error) {
	return readSourceMetadata(mountPoint)
}

func readSourceMetadata(mountPoint string) (*SourceMetadata, error) {
	data, err := os.ReadFile(sourceMetadataPath(mountPoint))
	if err != nil {
		return nil, err
	}
	var metadata SourceMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

func (s *AWSSnapshotter) saveSourceMetadata(mountPoint string, metadata SourceMetadata) error {
	metadataDir := filepath.Dir(sourceMetadataPath(mountPoint))
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return fmt.Errorf("failed to create source metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal source metadata: %w", err)
	}
	if err := os.WriteFile(sourceMetadataPath(mountPoint), data, 0644); err != nil {
		return fmt.Errorf("failed to write source metadata: %w", err)
	}
	return nil
}

func (s *AWSSnapshotter) deleteVolume(ctx context.Context, volumeID string) {
	if volumeID == "" {
		return
	}
	s.logger.Info().Msgf("Deleting volume %s...", volumeID)
	_, err := s.ec2Client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)})
	if err != nil {
		s.logger.Warn().Msgf("Failed to delete volume %s: %v. RunsOn TTL cleanup may remove it later.", volumeID, err)
		return
	}
	s.logger.Info().Msgf("Volume %s successfully deleted.", volumeID)
}

func (s *AWSSnapshotter) createVolumeClientToken(source string) *string {
	parts := []string{
		s.config.GithubRepository,
		s.config.GithubRef,
		s.config.Key,
		s.config.Path,
		source,
		os.Getenv("GITHUB_RUN_ID"),
		os.Getenv("GITHUB_RUN_ATTEMPT"),
		os.Getenv("GITHUB_JOB"),
		os.Getenv("GITHUB_ACTION"),
	}
	return aws.String("runs-on-" + hashValue(strings.Join(parts, "\x00"))[:56])
}

func sourceMetadataPath(mountPoint string) string {
	return filepath.Join(mountPoint, ".runs-on-snapshot", "source.json")
}

// runCommand executes a shell command and returns its combined output or an error.
// It now requires a context for potential cancellation if the command runs too long.
func (s *AWSSnapshotter) runCommand(ctx context.Context, name string, arg ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, arg...)
	s.logger.Info().Msgf("Executing command: %s %s", name, strings.Join(arg, " "))
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn().Msgf("Command failed: %s %s\nOutput:\n%s\nError: %v", name, strings.Join(arg, " "), string(output), err)
		return output, fmt.Errorf("command '%s %s' failed: %s: %w", name, strings.Join(arg, " "), string(output), err)
	}
	// Limit log output size for potentially verbose commands
	logOutput := string(output)
	if len(logOutput) > 400 {
		logOutput = logOutput[:200] + "... (output truncated)"
	}
	s.logger.Info().Msgf("Command successful. Output (first 200 chars or less):\n%s", logOutput)
	return output, nil
}

// getVolumeInfoPath returns the path to the volume info JSON file for a given mount point.
func (s *AWSSnapshotter) getVolumeInfoPath(mountPoint string) string {
	// Replace slashes with hyphens and remove leading/trailing hyphens
	sanitizedPath := strings.Trim(strings.ReplaceAll(mountPoint, "/", "-"), "-")
	return filepath.Join("/runs-on", fmt.Sprintf("snapshot-%s-%s-%s.json", sanitizedPath, hashValue(s.config.Key)[:12], hashValue(mountPoint)[:12]))
}

func hashValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func truncateTagValue(value string) string {
	const maxAWSValueLength = 256
	if len(value) <= maxAWSValueLength {
		return value
	}
	return value[:maxAWSValueLength]
}

func sanitizeNamePart(value string, maxLength int) string {
	value = strings.TrimPrefix(value, "refs/")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, " ", "-")
	if value == "" {
		value = "snapshot"
	}
	if len(value) > maxLength {
		value = value[:maxLength]
	}
	return value
}
