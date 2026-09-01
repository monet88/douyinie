package service_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/monet88/douyinie/internal/domain"
	"github.com/monet88/douyinie/internal/service"
)

func TestScrubPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		notMatch string
	}{
		{
			name:     "Windows drive path without CAS hash",
			input:    `C:\Users\monet\AppData\Local\Douyinie\data\file.mp4`,
			contains: "[REDACTED_PATH]",
			notMatch: `C:\Users`,
		},
		{
			name:     "Windows drive path with CAS hash",
			input:    `F:\CodeBase\douyinie\cas\ab\cd\abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890`,
			contains: "cas/ab/cd/abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
			notMatch: `F:\CodeBase`,
		},
		{
			name:     "Unix absolute path",
			input:    `/home/runner/work/douyinie/temp.wav`,
			contains: "[REDACTED_PATH]",
			notMatch: `/home/runner`,
		},
		{
			name:     "Windows UNC path",
			input:    `\\sharedserver\storage\clip.mp4`,
			contains: "[REDACTED_PATH]",
			notMatch: `\\sharedserver`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := service.ScrubPath(tc.input)
			if tc.contains != "" && !strings.Contains(got, tc.contains) {
				t.Errorf("expected %q to contain %q", got, tc.contains)
			}
			if tc.notMatch != "" && strings.Contains(got, tc.notMatch) {
				t.Errorf("expected %q not to contain %q", got, tc.notMatch)
			}
		})
	}
}

func TestScrubSecrets(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		notMatch string
	}{
		{
			name:     "OpenAI key sk-",
			input:    "config has sk-1234567890abcdef1234567890abcdef here",
			notMatch: "sk-1234567890",
		},
		{
			name:     "GitHub token ghp_",
			input:    "using ghp_1234567890abcdefghijklmnopqrstuvwxyz",
			notMatch: "ghp_1234567890",
		},
		{
			name:     "AWS key AKIA",
			input:    "AWS_KEY=AKIAIOSFODNN7EXAMPLE",
			notMatch: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:     "JWT token",
			input:    `{"token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"}`,
			notMatch: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
		{
			name:     "api_key param",
			input:    `{"api_key": "mySecretPassword12345"}`,
			notMatch: "mySecretPassword12345",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := service.ScrubSecrets(tc.input)
			if tc.notMatch != "" && strings.Contains(got, tc.notMatch) {
				t.Errorf("expected %q not to contain %q", got, tc.notMatch)
			}
		})
	}
}

func TestScrubManifest_Validation(t *testing.T) {
	manifest := &domain.JobBundleManifest{
		BundleVersion: 1,
		ExportedAt:    time.Now().UTC(),
		Job: domain.LocalizationJob{
			ID:             "job-1",
			SourceAssetID:  "asset-1",
			TargetLanguage: "vi",
			Status:         "completed",
		},
		SourceAsset: domain.SourceAsset{
			ID:               "asset-1",
			SHA256:           "4b971e41be8a65fa6319808a7beee79b9b1e5f8f8b03126ab56627045b85a3c0",
			ByteSize:         1000,
			OriginalFilename: `C:\Users\monet\Videos\sample.mp4`,
			CASPath:          `F:\CodeBase\douyinie\cas\4b\97\4b971e41be8a65fa6319808a7beee79b9b1e5f8f8b03126ab56627045b85a3c0`,
		},
		RightsAttestation: &domain.RightsAttestation{
			DeclaredBy: "operator-1",
			Notes:      "Contains secret token: api_key=superSecretKey12345 and path C:\\data\\source",
		},
		LicenseManifests: []domain.LicenseManifestEntry{
			{
				DependencyName: "qwen3-asr",
				Version:        "1.7b",
				SHA256:         "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
				CodeLicense:    "Apache-2.0",
				ModelLicense:   "Qwen-Research",
				DataLicense:    "Common-Voice",
				ServiceTerms:   "Local-Offline",
				Verified:       true,
			},
		},
		Runs: []domain.JobBundleRunData{
			{
				Run: domain.LocalizationRun{
					ID:                 "run-1",
					JobID:              "job-1",
					ConfigSnapshotJSON: `{"api_key": "secretToken12345", "temp_dir": "C:\\temp\\run1"}`,
				},
			},
		},
	}

	// Before scrubbing, validation MUST fail
	errBefore := service.ValidateBundleManifest(manifest)
	if errBefore == nil {
		t.Fatalf("expected validation error before scrubbing, got nil")
	}

	// Apply ScrubManifest
	service.ScrubManifest(manifest)

	// After scrubbing, OriginalFilename should be "sample.mp4"
	if manifest.SourceAsset.OriginalFilename != "sample.mp4" {
		t.Errorf("expected sample.mp4, got %q", manifest.SourceAsset.OriginalFilename)
	}

	// CASPath should be relative cas/4b/97/4b971...
	if strings.Contains(manifest.SourceAsset.CASPath, `F:\CodeBase`) {
		t.Errorf("CASPath still contains machine-local path: %q", manifest.SourceAsset.CASPath)
	}

	// Now validation MUST pass
	if err := service.ValidateBundleManifest(manifest); err != nil {
		t.Fatalf("validation failed after scrubbing: %v", err)
	}
}

func TestValidateManifest_MissingLicenseLayers(t *testing.T) {
	manifest := &domain.JobBundleManifest{
		BundleVersion: 1,
		Job: domain.LocalizationJob{
			ID: "job-1",
		},
		SourceAsset: domain.SourceAsset{
			ID: "asset-1",
		},
		LicenseManifests: []domain.LicenseManifestEntry{
			{
				DependencyName: "partial-dep",
				Version:        "1.0",
				CodeLicense:    "MIT",
				// Missing ModelLicense, DataLicense, ServiceTerms
			},
		},
	}

	err := service.ValidateBundleManifest(manifest)
	if !errors.Is(err, domain.ErrJobBundleLicenseIncomplete) {
		t.Fatalf("expected ErrJobBundleLicenseIncomplete, got %v", err)
	}
}

