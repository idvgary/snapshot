package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/rs/zerolog"
	"github.com/runs-on/snapshot/internal/config"
	"github.com/runs-on/snapshot/internal/snapshot"
	"github.com/sethvargo/go-githubactions"
)

// handleMainExecution contains the original main logic.
func handleMainExecution(action *githubactions.Action, ctx context.Context, logger *zerolog.Logger) {
	cfg := config.NewConfigFromInputs(action)

	if cfg.Path != "" {
		action.Infof("Restoring volume for %s...", cfg.Path)
		snapshotter, err := snapshot.NewAWSSnapshotter(ctx, logger, cfg)
		if err != nil {
			action.Fatalf("Failed to create snapshotter: %v", err)
		} else {
			action.Infof("Creating snapshot for %s", cfg.Path)
			snapshotOutput, err := snapshotter.RestoreSnapshot(ctx, cfg.Path)
			if err != nil {
				action.Fatalf("Failed to restore snapshot for %s: %v", cfg.Path, err)
			} else {
				action.Infof("Snapshot restored into volume %s", snapshotOutput.VolumeID)
				action.SetOutput("restored", strconv.FormatBool(snapshotOutput.Restored))
				action.SetOutput("restored-from", snapshotOutput.RestoredFrom)
				action.SetOutput("restored-branch", snapshotOutput.RestoredBranch)
				action.SetOutput("restored-snapshot-id", snapshotOutput.RestoredSnapshotID)
				action.SetOutput("volume-id", snapshotOutput.VolumeID)
				action.SetOutput("restored-source-sha", snapshotOutput.RestoredSourceSHA)
				action.SetOutput("restored-source-ref", snapshotOutput.RestoredSourceRef)
			}
		}
	}

	action.Infof("Action finished.")
}

// handlePostExecution contains the logic for the post-execution phase.
func handlePostExecution(action *githubactions.Action, ctx context.Context, logger *zerolog.Logger) {
	action.Infof("Running post-execution phase...")
	cfg := config.NewConfigFromInputs(action)

	if !cfg.Save {
		action.Infof("Skipping snapshot creation as 'save' is set to false; cleaning up restored volume.")
		snapshotter, err := snapshot.NewAWSSnapshotter(ctx, logger, cfg)
		if err != nil {
			action.Fatalf("Failed to create snapshotter: %v", err)
		}
		if err := snapshotter.CleanupVolume(ctx, cfg.Path); err != nil {
			action.Fatalf("Failed to cleanup volume for %s: %v", cfg.Path, err)
		}
		action.Infof("Post-execution phase finished.")
		return
	}

	if cfg.Path != "" {
		action.Infof("Snapshotting volume for %s...", cfg.Path)
		snapshotter, err := snapshot.NewAWSSnapshotter(ctx, logger, cfg)
		if err != nil {
			action.Fatalf("Failed to create snapshotter: %v", err)
		} else {
			snapshot, err := snapshotter.CreateSnapshot(ctx, cfg.Path)
			if err != nil {
				action.Fatalf("Failed to snapshot volumes: %v", err)
			} else if snapshot.Skipped {
				action.Infof("Snapshot save skipped: %s", snapshot.SkipReason)
				writeStepSummary("Snapshot save skipped", snapshot.SkipReason, snapshot.BaseSourceSHA, snapshot.SourceSHA)
			} else {
				action.Infof("Snapshot created: %s. Note that it might take a few minutes to be available for use.", snapshot.SnapshotID)
				writeStepSummary("Snapshot save started", snapshot.SaveReason, snapshot.BaseSourceSHA, snapshot.SourceSHA)
			}
		}
	}
	action.Infof("Post-execution phase finished.")
}

func writeStepSummary(title string, reason string, baseSHA string, sourceSHA string) {
	summaryPath := os.Getenv("GITHUB_STEP_SUMMARY")
	if summaryPath == "" {
		return
	}
	content := fmt.Sprintf("### %s\n\n- Reason: `%s`\n- Base source SHA: `%s`\n- Current source SHA: `%s`\n\n", title, reason, baseSHA, sourceSHA)
	file, err := os.OpenFile(summaryPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.WriteString(content)
}

func main() {
	ctx := context.Background()
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()
	postFlag := flag.Bool("post", false, "Indicates the post-execution phase")
	flag.Parse()

	action := githubactions.New()

	if *postFlag {
		handlePostExecution(action, ctx, &logger)
	} else {
		handleMainExecution(action, ctx, &logger)
	}
}
