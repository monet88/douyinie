package service

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/monet88/douyinie/internal/domain"
)

var (
	// Windows drive paths: C:\..., D:/... (word boundary before drive letter, single slash or backslash, not URL scheme ://)
	winPathRegex = regexp.MustCompile(`(?i)\b[a-zA-Z]:[\\/](?:[^/]|$)[\w\-\.\\/]*`)
	// Windows UNC paths: \\server\share\...
	uncPathRegex = regexp.MustCompile(`\\\\[^"'\s\t\r\n,;}{]+`)
	// Unix system absolute paths: /Users/..., /home/..., /tmp/..., etc.
	unixPathRegex = regexp.MustCompile(`/(?:Users|home|tmp|var|private|etc|opt|usr|root|mnt|media)/[^"'\s\t\r\n,;}{]+`)

	apiKeyQuotedRegex   = regexp.MustCompile(`(?i)["']?(api[_-]?key|access[_-]?token|secret|password|bearer|auth[_-]?token|credential)["']?\s*[=:]\s*["']([^"'\r\n]+)["']`)
	apiKeyUnquotedRegex = regexp.MustCompile(`(?i)["']?(api[_-]?key|access[_-]?token|secret|password|bearer|auth[_-]?token|credential)["']?\s*[=:]\s*([^\s"'` + "`" + `\r\n,;}{]+)`)
	apiKeyParamRegexes  = []*regexp.Regexp{apiKeyQuotedRegex, apiKeyUnquotedRegex}
	apiKeyParamRegex    = apiKeyQuotedRegex
	skKeyRegex          = regexp.MustCompile(`sk-[a-zA-Z0-9_\-]{20,}`)
	ghpKeyRegex         = regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`)
	awsKeyRegex         = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	bearerRegex         = regexp.MustCompile(`(?i)bearer\s+([^\s"'` + "`" + `\r\n,;}{]{15,})`)
	jwtRegex            = regexp.MustCompile(`eyJ[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]{10,}\.[a-zA-Z0-9_\-]+`)
)

// ScrubPath replaces machine-local absolute paths with portable representations.
func ScrubPath(s string) string {
	if s == "" {
		return ""
	}

	// 1. If it contains a CAS hash (64 hex chars), extract and normalize to relative cas/<hash>
	casMatch := regexp.MustCompile(`(?i)[0-9a-f]{64}`).FindString(s)
	if casMatch != "" && (winPathRegex.MatchString(s) || unixPathRegex.MatchString(s) || uncPathRegex.MatchString(s)) {
		lowerHash := strings.ToLower(casMatch)
		return "cas/" + lowerHash[0:2] + "/" + lowerHash[2:4] + "/" + lowerHash
	}

	res := winPathRegex.ReplaceAllString(s, "[REDACTED_PATH]")
	res = uncPathRegex.ReplaceAllString(res, "[REDACTED_PATH]")
	res = unixPathRegex.ReplaceAllString(res, "[REDACTED_PATH]")
	return res
}

// ScrubSecrets redacts sensitive credentials, API keys, tokens, and passwords.
func ScrubSecrets(s string) string {
	if s == "" {
		return ""
	}

	res := skKeyRegex.ReplaceAllString(s, "[REDACTED_SECRET]")
	res = ghpKeyRegex.ReplaceAllString(res, "[REDACTED_SECRET]")
	res = awsKeyRegex.ReplaceAllString(res, "[REDACTED_SECRET]")
	res = jwtRegex.ReplaceAllString(res, "[REDACTED_SECRET]")
	res = bearerRegex.ReplaceAllString(res, "Bearer [REDACTED_SECRET]")

	// Scrub key=value or "key": "value" patterns
	for _, re := range apiKeyParamRegexes {
		res = re.ReplaceAllStringFunc(res, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if len(sub) >= 3 {
				val := sub[2]
				if val != "[REDACTED_SECRET]" && !strings.Contains(val, "REDACTED") && val != "null" && val != "true" && val != "false" {
					return strings.Replace(m, val, "[REDACTED_SECRET]", 1)
				}
			}
			return m
		})
	}

	return res
}

// ScrubText performs combined path and secret scrubbing on text.
func ScrubText(s string) string {
	return ScrubSecrets(ScrubPath(s))
}

// ScrubManifest sanitizes a JobBundleManifest in-place to remove all secrets and machine-local absolute paths.
func ScrubManifest(m *domain.JobBundleManifest) {
	if m == nil {
		return
	}

	// 1. SourceAsset
	if m.SourceAsset.OriginalFilename != "" {
		m.SourceAsset.OriginalFilename = filepath.Base(filepath.ToSlash(m.SourceAsset.OriginalFilename))
	}
	m.SourceAsset.CASPath = ScrubPath(m.SourceAsset.CASPath)

	// 2. RightsAttestation
	if m.RightsAttestation != nil {
		m.RightsAttestation.DeclaredBy = ScrubText(m.RightsAttestation.DeclaredBy)
		m.RightsAttestation.Notes = ScrubText(m.RightsAttestation.Notes)
	}

	// 3. PreflightReport
	if m.PreflightReport != nil {
		m.PreflightReport.NormalizedAudioCASPath = ScrubPath(m.PreflightReport.NormalizedAudioCASPath)
		for i, errStr := range m.PreflightReport.Errors {
			m.PreflightReport.Errors[i] = ScrubText(errStr)
		}
	}

	// 4. AudioStems
	if m.AudioStems != nil {
		for i := range m.AudioStems.Stems {
			m.AudioStems.Stems[i].AudioCASPath = ScrubPath(m.AudioStems.Stems[i].AudioCASPath)
		}
	}

	// 5. Runs
	for rIdx := range m.Runs {
		runData := &m.Runs[rIdx]

		// Config snapshot JSON
		if runData.Run.ConfigSnapshotJSON != "" {
			runData.Run.ConfigSnapshotJSON = ScrubText(runData.Run.ConfigSnapshotJSON)
		}

		// Stage executions
		for sIdx := range runData.StageExecutions {
			runData.StageExecutions[sIdx].ErrorMessage = ScrubText(runData.StageExecutions[sIdx].ErrorMessage)
		}

		// Provider attempts
		for aIdx := range runData.ProviderAttempts {
			runData.ProviderAttempts[aIdx].ErrorMessage = ScrubText(runData.ProviderAttempts[aIdx].ErrorMessage)
		}

		// Selection decisions
		for dIdx := range runData.SelectionDecisions {
			runData.SelectionDecisions[dIdx].DecisionReason = ScrubText(runData.SelectionDecisions[dIdx].DecisionReason)
		}

		// DubSegmentsVariant
		if runData.DubSegmentsVariant != nil {
			for segIdx := range runData.DubSegmentsVariant.Segments {
				seg := &runData.DubSegmentsVariant.Segments[segIdx]
				seg.AudioCASPath = ScrubPath(seg.AudioCASPath)
			}
			for revIdx := range runData.DubSegmentsVariant.ReviewSegments {
				rev := &runData.DubSegmentsVariant.ReviewSegments[revIdx]
				rev.AudioCASPath = ScrubPath(rev.AudioCASPath)
			}
		}

		// DubMixArtifact
		if runData.DubMixArtifact != nil {
			runData.DubMixArtifact.AudioCASPath = ScrubPath(runData.DubMixArtifact.AudioCASPath)
		}

		// RenderArtifact
		if runData.RenderArtifact != nil {
			if runData.RenderArtifact.Preview != nil {
				runData.RenderArtifact.Preview.OutputCASPath = ScrubPath(runData.RenderArtifact.Preview.OutputCASPath)
			}
			if runData.RenderArtifact.Final != nil {
				runData.RenderArtifact.Final.OutputCASPath = ScrubPath(runData.RenderArtifact.Final.OutputCASPath)
			}
		}
	}
}

// ValidateManifestJSON scans raw manifest JSON bytes to fail-closed if any prohibited secret or
// machine-local absolute path is present, or if license obligation layers are incomplete.
func ValidateManifestJSON(data []byte) error {
	s := string(data)

	// 1. Prohibited machine-local absolute paths
	if match := winPathRegex.FindString(s); match != "" {
		return fmt.Errorf("%w: matched Windows path %q", domain.ErrJobBundleMachineLocalPath, match)
	}
	if match := uncPathRegex.FindString(s); match != "" {
		return fmt.Errorf("%w: matched UNC path %q", domain.ErrJobBundleMachineLocalPath, match)
	}
	if match := unixPathRegex.FindString(s); match != "" {
		return fmt.Errorf("%w: matched Unix path %q", domain.ErrJobBundleMachineLocalPath, match)
	}

	// 2. Prohibited secret credentials
	if skKeyRegex.MatchString(s) || ghpKeyRegex.MatchString(s) || awsKeyRegex.MatchString(s) || jwtRegex.MatchString(s) {
		return domain.ErrJobBundleSecretDetected
	}

	for _, match := range bearerRegex.FindAllStringSubmatch(s, -1) {
		if len(match) >= 2 {
			val := match[1]
			if val != "[REDACTED_SECRET]" && !strings.Contains(val, "REDACTED") {
				return domain.ErrJobBundleSecretDetected
			}
		}
	}

	// Check for unredacted password/secret/api_key values
	for _, re := range apiKeyParamRegexes {
		matches := re.FindAllStringSubmatch(s, -1)
		for _, match := range matches {
			if len(match) >= 3 {
				val := match[2]
				if val != "[REDACTED_SECRET]" && !strings.Contains(val, "REDACTED") && val != "null" && val != "true" && val != "false" {
					return domain.ErrJobBundleSecretDetected
				}
			}
		}
	}

	// 3. License obligation layers check
	var partial struct {
		LicenseManifests []domain.LicenseManifestEntry `json:"license_manifests"`
	}
	if err := json.Unmarshal(data, &partial); err == nil {
		for _, lm := range partial.LicenseManifests {
			if strings.TrimSpace(lm.CodeLicense) == "" ||
				strings.TrimSpace(lm.ModelLicense) == "" ||
				strings.TrimSpace(lm.DataLicense) == "" ||
				strings.TrimSpace(lm.ServiceTerms) == "" {
				return domain.ErrJobBundleLicenseIncomplete
			}
		}
	}

	return nil
}

// ValidateBundleManifest validates a typed JobBundleManifest.
func ValidateBundleManifest(m *domain.JobBundleManifest) error {
	if m == nil {
		return domain.ErrJobBundleInvalid
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return ValidateManifestJSON(data)
}
