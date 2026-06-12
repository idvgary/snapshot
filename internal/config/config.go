package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/runs-on/snapshot/internal/utils"
	"github.com/sethvargo/go-githubactions"
)

const requiredTagKey = "runs-on-stack-name"

type Config struct {
	Path                     string
	Key                      string
	RestoreKeys              []string
	Version                  string
	WaitForCompletion        bool
	Save                     bool
	SaveMode                 string
	SaveIf                   string
	ForceSave                bool
	SaveOnEmpty              bool
	WaitForCleanup           bool
	SavePolicyName           string
	SavePolicyVersion        string
	SaveMarkerFile           string
	GitRepository            string
	GitHead                  string
	GitPaths                 []string
	RetentionDays            int32
	KeepLastSnapshots        int32
	DefaultBranchFallback    bool
	VolumeType               types.VolumeType
	VolumeIops               int32
	VolumeThroughput         int32
	VolumeSize               int32
	VolumeInitializationRate int32
	VolumeName               string
	GithubRef                string
	GithubRepository         string
	InstanceID               string
	Az                       string
	CustomTags               []Tag
	SnapshotName             string
	RunnerConfig             *RunnerConfig
}

type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type RunnerConfig struct {
	DefaultBranch string `json:"defaultBranch"`
	CustomTags    []Tag  `json:"customTags"`
}

// NewConfigFromInputs parses action inputs and environment variables to build the Config struct.
func NewConfigFromInputs(action *githubactions.Action) *Config {
	cfg := &Config{
		GithubRef:        os.Getenv("GITHUB_REF_NAME"),
		GithubRepository: os.Getenv("GITHUB_REPOSITORY"),
		InstanceID:       os.Getenv("RUNS_ON_INSTANCE_ID"),
		Az:               os.Getenv("RUNS_ON_AWS_AZ"),
	}

	configBytes, err := os.ReadFile(filepath.Join(os.Getenv("RUNS_ON_HOME"), "config.json"))
	if err != nil {
		action.Fatalf("Error reading RunsOn config file: %v. You must be using RunsOn v2.8.3+", err)
	} else {
		var runnerConfig RunnerConfig
		if err := json.Unmarshal(configBytes, &runnerConfig); err != nil {
			action.Fatalf("Error parsing RunsOn config file: %v", err)
		} else {
			cfg.RunnerConfig = &runnerConfig
			action.Infof("Runner config: %s", utils.PrettyPrint(cfg.RunnerConfig))
		}
	}

	requiredTagPresent := false
	for _, tag := range cfg.RunnerConfig.CustomTags {
		if tag.Key == requiredTagKey {
			requiredTagPresent = true
		}
		cfg.CustomTags = append(cfg.CustomTags, Tag{
			Key:   tag.Key,
			Value: tag.Value,
		})
	}

	if !requiredTagPresent {
		action.Fatalf("Required tag '%s' is not present in the RunsOn config file.", requiredTagKey)
	}

	path := action.GetInput("path")
	path = strings.TrimSpace(path)
	if path == "" {
		action.Fatalf("Path is required.")
	}
	if !strings.HasPrefix(path, "/") {
		action.Fatalf("Path '%s' must be an absolute path.", path)
	}
	cfg.Path = path

	key := strings.TrimSpace(action.GetInput("key"))
	if key == "" {
		action.Fatalf("Key is required.")
	}
	cfg.Key = key
	cfg.RestoreKeys = parseRestoreKeys(action.GetInput("restore-keys"))

	cfg.Version = action.GetInput("version")
	if cfg.Version == "" {
		cfg.Version = "v1"
	}

	cfg.WaitForCompletion = parseBoolInput(action, "wait_for_completion", false)
	saveInput := strings.TrimSpace(action.GetInput("save"))
	if saveInput == "" {
		saveInput = "true"
	}
	cfg.SaveMode = saveInput
	cfg.Save = saveInput != "false"
	cfg.SaveIf = strings.TrimSpace(action.GetInput("save-if"))
	if cfg.SaveIf == "" {
		cfg.SaveIf = "always"
	}
	if cfg.SaveMode != "true" && cfg.SaveMode != "false" && cfg.SaveMode != "auto" {
		action.Fatalf("Invalid value for 'save': %s. Expected true, false, or auto.", cfg.SaveMode)
	}
	if cfg.SaveMode == "auto" && cfg.SaveIf != "always" && cfg.SaveIf != "git-paths-changed" {
		action.Fatalf("Invalid value for 'save-if': %s. Expected always or git-paths-changed.", cfg.SaveIf)
	}
	cfg.ForceSave = parseBoolInput(action, "force-save", false)
	cfg.SaveOnEmpty = parseBoolInput(action, "save-on-empty", true)
	cfg.WaitForCleanup = parseBoolInput(action, "wait-for-cleanup", true)
	cfg.SavePolicyName = strings.TrimSpace(action.GetInput("save-policy-name"))
	cfg.SavePolicyVersion = strings.TrimSpace(action.GetInput("save-policy-version"))
	cfg.SaveMarkerFile = strings.TrimSpace(action.GetInput("save-marker-file"))
	cfg.GitRepository = strings.TrimSpace(action.GetInput("git-repository"))
	cfg.GitHead = strings.TrimSpace(action.GetInput("git-head"))
	if cfg.GitHead == "" {
		cfg.GitHead = os.Getenv("GITHUB_SHA")
	}
	cfg.GitPaths = parseRestoreKeys(action.GetInput("git-paths"))
	cfg.DefaultBranchFallback = action.GetInput("default-branch-fallback") != "false"

	volumeType := strings.ToLower(strings.TrimSpace(action.GetInput("volume_type")))
	if volumeType == "" {
		volumeType = "gp3"
	}
	cfg.VolumeType = types.VolumeType(volumeType)

	cfg.VolumeInitializationRate = parseInt(action, "volume_initialization_rate", 0, 0)
	cfg.VolumeIops = parseInt(action, "volume_iops", 100, 0)
	cfg.VolumeThroughput = parseInt(action, "volume_throughput", 100, 0)
	cfg.VolumeSize = parseInt(action, "volume_size", 1, 0)
	cfg.RetentionDays = parseInt(action, "retention-days", 0, 0)
	cfg.KeepLastSnapshots = parseInt(action, "keep-last-snapshots", 0, 0)
	if err := validateEBSVolumeConfig(cfg); err != nil {
		action.Fatalf("Invalid EBS volume configuration: %v", err)
	}

	action.Infof("Input 'path': %v", cfg.Path)
	action.Infof("Input 'key': %s", cfg.Key)
	action.Infof("Input 'version': %s", cfg.Version)
	action.Infof("Input 'save': %s", cfg.SaveMode)
	action.Infof("Input 'save-if': %s", cfg.SaveIf)
	action.Infof("Input 'force-save': %t", cfg.ForceSave)
	action.Infof("Input 'save-on-empty': %t", cfg.SaveOnEmpty)
	action.Infof("Input 'wait-for-cleanup': %t", cfg.WaitForCleanup)
	action.Infof("Input 'wait_for_completion': %t", cfg.WaitForCompletion)
	action.Infof("Input 'default-branch-fallback': %t", cfg.DefaultBranchFallback)

	return cfg
}

func validateEBSVolumeConfig(cfg *Config) error {
	if cfg.VolumeInitializationRate != 0 {
		if err := validateRange("volume_initialization_rate", cfg.VolumeInitializationRate, 100, 300); err != nil {
			return err
		}
	}

	switch cfg.VolumeType {
	case types.VolumeTypeGp3:
		if err := validateRange("volume_size", cfg.VolumeSize, 1, 65536); err != nil {
			return err
		}
		if err := validateRange("volume_iops", cfg.VolumeIops, 3000, 80000); err != nil {
			return err
		}
		if err := validateRange("volume_throughput", cfg.VolumeThroughput, 125, 2000); err != nil {
			return err
		}
		if cfg.VolumeThroughput*4 > cfg.VolumeIops {
			return fmt.Errorf("volume_throughput must not exceed 0.25 MiB/s per provisioned gp3 IOPS, got %d MiB/s for %d IOPS", cfg.VolumeThroughput, cfg.VolumeIops)
		}
		return nil
	case types.VolumeTypeGp2:
		return validateRange("volume_size", cfg.VolumeSize, 1, 16384)
	case types.VolumeTypeIo1:
		if err := validateRange("volume_size", cfg.VolumeSize, 4, 16384); err != nil {
			return err
		}
		return validateRange("volume_iops", cfg.VolumeIops, 100, 64000)
	case types.VolumeTypeIo2:
		if err := validateRange("volume_size", cfg.VolumeSize, 4, 65536); err != nil {
			return err
		}
		return validateRange("volume_iops", cfg.VolumeIops, 100, 256000)
	case types.VolumeTypeSt1, types.VolumeTypeSc1:
		return validateRange("volume_size", cfg.VolumeSize, 125, 16384)
	case types.VolumeTypeStandard:
		return validateRange("volume_size", cfg.VolumeSize, 1, 1024)
	default:
		return fmt.Errorf("volume_type must be one of standard, gp2, gp3, io1, io2, st1, or sc1, got %q", cfg.VolumeType)
	}
}

func validateRange(name string, value int32, min int32, max int32) error {
	if value < min || value > max {
		return fmt.Errorf("%s must be between %d and %d, got %d", name, min, max, value)
	}
	return nil
}

func parseBoolInput(action *githubactions.Action, input string, defaultValue bool) bool {
	value := strings.TrimSpace(action.GetInput(input))
	if value == "" {
		return defaultValue
	}
	switch strings.ToLower(value) {
	case "true":
		return true
	case "false":
		return false
	default:
		action.Fatalf("Invalid boolean value for '%s': %s", input, value)
		return defaultValue
	}
}

func parseRestoreKeys(input string) []string {
	var keys []string
	for _, line := range strings.Split(input, "\n") {
		key := strings.TrimSpace(line)
		if key == "" {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func parseInt(action *githubactions.Action, input string, min int, max int) int32 {
	value := action.GetInput(input)
	if value == "" {
		action.Fatalf("'%s' cannot be empty", input)
	}
	valueInt, err := strconv.Atoi(value)
	if err != nil {
		action.Fatalf("Invalid value '%s': %v", value, err)
	}
	if valueInt < min {
		action.Fatalf("Invalid value '%s': must be at least %d", value, min)
	}
	if max > 0 && valueInt > max {
		action.Fatalf("Invalid value '%s': must be at most %d", value, max)
	}
	return int32(valueInt)
}
