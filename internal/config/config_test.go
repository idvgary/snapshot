package config

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestValidateEBSVolumeConfigAcceptsDefaultGp3(t *testing.T) {
	cfg := &Config{
		VolumeType:       types.VolumeTypeGp3,
		VolumeSize:       40,
		VolumeIops:       3000,
		VolumeThroughput: 750,
	}

	if err := validateEBSVolumeConfig(cfg); err != nil {
		t.Fatalf("expected default gp3 config to be valid: %v", err)
	}
}

func TestValidateEBSVolumeConfigRejectsInvalidGp3Throughput(t *testing.T) {
	cfg := &Config{
		VolumeType:       types.VolumeTypeGp3,
		VolumeSize:       40,
		VolumeIops:       3000,
		VolumeThroughput: 100,
	}

	err := validateEBSVolumeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "volume_throughput") {
		t.Fatalf("expected throughput validation error, got %v", err)
	}
}

func TestValidateEBSVolumeConfigRejectsInvalidGp3ThroughputToIopsRatio(t *testing.T) {
	cfg := &Config{
		VolumeType:       types.VolumeTypeGp3,
		VolumeSize:       40,
		VolumeIops:       3000,
		VolumeThroughput: 751,
	}

	err := validateEBSVolumeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "0.25") {
		t.Fatalf("expected throughput-to-IOPS validation error, got %v", err)
	}
}

func TestValidateEBSVolumeConfigRejectsInvalidInitializationRate(t *testing.T) {
	cfg := &Config{
		VolumeType:               types.VolumeTypeGp3,
		VolumeSize:               40,
		VolumeIops:               3000,
		VolumeThroughput:         750,
		VolumeInitializationRate: 99,
	}

	err := validateEBSVolumeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "volume_initialization_rate") {
		t.Fatalf("expected initialization-rate validation error, got %v", err)
	}
}

func TestValidateEBSVolumeConfigRejectsUnknownVolumeType(t *testing.T) {
	cfg := &Config{
		VolumeType:       types.VolumeType("banana"),
		VolumeSize:       40,
		VolumeIops:       3000,
		VolumeThroughput: 750,
	}

	err := validateEBSVolumeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "volume_type") {
		t.Fatalf("expected volume type validation error, got %v", err)
	}
}

func TestValidateEBSVolumeConfigRejectsTooSmallSt1Volume(t *testing.T) {
	cfg := &Config{
		VolumeType: types.VolumeTypeSt1,
		VolumeSize: 40,
	}

	err := validateEBSVolumeConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "volume_size") {
		t.Fatalf("expected volume size validation error, got %v", err)
	}
}
