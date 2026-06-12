package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	runsOnConfig "github.com/runs-on/snapshot/internal/config"
)

func TestDefaultTagsIncludesSnapshotRepositoryAndRepoFullName(t *testing.T) {
	const repository = "owner/repo"

	snapshotter := &AWSSnapshotter{
		config: &runsOnConfig.Config{
			Version:          "v1",
			Key:              "example-build-release-arm64",
			Path:             "/mnt/snapshot-a",
			GithubRepository: repository,
			GithubRef:        "main",
		},
	}

	tags := tagsByKey(snapshotter.defaultTags())

	for _, key := range []string{snapshotTagKeyRepository, repoFullNameTagKey} {
		if got := tags[key]; got != repository {
			t.Fatalf("expected tag %s to be %q, got %q", key, repository, got)
		}
	}
}

func TestIdentityTagsIncludeKeyAndPathHashes(t *testing.T) {
	snapshotter := &AWSSnapshotter{
		config: &runsOnConfig.Config{
			Version:          "v3",
			Key:              "example-build-release-arm64",
			Path:             "/mnt/example-cargo-snapshot",
			GithubRepository: "owner/repo",
			GithubRef:        "feature/test",
		},
	}

	tags := tagsByKey(snapshotter.defaultTags())

	if got := tags[snapshotTagKeyKey]; got != "example-build-release-arm64" {
		t.Fatalf("expected readable key tag, got %q", got)
	}
	if got := tags[snapshotTagKeyKeyHash]; got != hashValue("example-build-release-arm64") {
		t.Fatalf("expected key hash tag %q, got %q", hashValue("example-build-release-arm64"), got)
	}
	if got := tags[snapshotTagKeyPathHash]; got != hashValue("/mnt/example-cargo-snapshot") {
		t.Fatalf("expected path hash tag %q, got %q", hashValue("/mnt/example-cargo-snapshot"), got)
	}
}

func TestVolumeInfoPathIncludesKeyAndPathIdentity(t *testing.T) {
	snapshotterA := &AWSSnapshotter{config: &runsOnConfig.Config{Key: "build-a"}}
	snapshotterB := &AWSSnapshotter{config: &runsOnConfig.Config{Key: "build-b"}}

	pathA := snapshotterA.getVolumeInfoPath("/mnt/snapshot")
	pathB := snapshotterB.getVolumeInfoPath("/mnt/snapshot")

	if pathA == pathB {
		t.Fatalf("expected different volume info paths for different keys, got %q", pathA)
	}
}

func TestSourceMetadataPathIsUnderMountPoint(t *testing.T) {
	got := sourceMetadataPath("/mnt/snapshot")
	want := "/mnt/snapshot/.runs-on-snapshot/source.json"
	if got != want {
		t.Fatalf("expected source metadata path %q, got %q", want, got)
	}
}

func TestNextAvailableAttachmentDeviceNameSkipsUsedDevices(t *testing.T) {
	got, err := nextAvailableAttachmentDeviceName(map[string]bool{"/dev/sdf": true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/sdg" {
		t.Fatalf("expected /dev/sdg, got %q", got)
	}
}

func TestNextAvailableAttachmentDeviceNameErrorsWhenExhausted(t *testing.T) {
	used := make(map[string]bool, len(attachmentDeviceCandidates))
	for _, candidate := range attachmentDeviceCandidates {
		used[candidate] = true
	}
	if _, err := nextAvailableAttachmentDeviceName(used); err == nil {
		t.Fatalf("expected exhausted attachment candidates to fail")
	}
}

func TestCreateVolumeClientTokenIsStableAndBounded(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "123")
	t.Setenv("GITHUB_RUN_ATTEMPT", "2")
	t.Setenv("GITHUB_JOB", "build")
	t.Setenv("GITHUB_ACTION", "cargo-snapshot")
	snapshotter := &AWSSnapshotter{config: &runsOnConfig.Config{
		GithubRepository: "owner/repo",
		GithubRef:        "feature/test",
		Key:              "example-build-release-arm64",
		Path:             "/mnt/example-cargo-snapshot",
	}}

	first := snapshotter.createVolumeClientToken("snapshot:snap-123")
	second := snapshotter.createVolumeClientToken("snapshot:snap-123")
	differentSource := snapshotter.createVolumeClientToken("blank")

	if first == nil || second == nil || *first != *second {
		t.Fatalf("expected stable client token, got %v and %v", first, second)
	}
	if len(*first) > 64 {
		t.Fatalf("expected EC2 client token to be <= 64 bytes, got %d", len(*first))
	}
	if *first == *differentSource {
		t.Fatalf("expected different volume sources to use different client tokens")
	}
}

func TestSourceTagsIncludeMetadataAndRetention(t *testing.T) {
	snapshotter := &AWSSnapshotter{config: &runsOnConfig.Config{RetentionDays: 10}}
	tags := tagsByKey(snapshotter.sourceTags(SourceMetadata{
		SHA:               "abc123",
		Ref:               "main",
		SavePolicyName:    "changed-source",
		SavePolicyVersion: "v1",
		SavedAt:           time.Now(),
	}))

	for key, want := range map[string]string{
		snapshotTagKeySourceSHA: "abc123",
		snapshotTagKeySourceRef: "main",
		snapshotTagKeyPolicy:    "changed-source",
		snapshotTagKeyPolicyVer: "v1",
	} {
		if got := tags[key]; got != want {
			t.Fatalf("expected tag %s to be %q, got %q", key, want, got)
		}
	}
	if tags[ttlTagKey] == "" {
		t.Fatalf("expected retention tag %s", ttlTagKey)
	}
}

func TestSaveMarkerCanSkipSave(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "save-marker")
	if err := os.WriteFile(markerPath, []byte("save=false\n"), 0644); err != nil {
		t.Fatal(err)
	}
	decision, err := ShouldSaveSnapshot(context.Background(), &runsOnConfig.Config{
		SaveMode:       "auto",
		SaveMarkerFile: markerPath,
	}, tmpDir, func(ctx context.Context, name string, arg ...string) ([]byte, error) {
		t.Fatalf("did not expect command execution")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Skipped() {
		t.Fatalf("expected save marker to skip save")
	}
}

func tagsByKey(tags []types.Tag) map[string]string {
	result := make(map[string]string, len(tags))
	for _, tag := range tags {
		if tag.Key == nil || tag.Value == nil {
			continue
		}
		result[*tag.Key] = *tag.Value
	}
	return result
}
