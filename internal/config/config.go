package config

import (
	"encoding/json"
	"fmt"

	"github.com/monet88/douyinie/internal/domain"
)

// DefaultLayeredConfig returns the application base defaults.
// Invariants: Hybrid profile default, Telemetry disabled by default, ZeroOverrunStrict = true, MaxRetries = 2.
func DefaultLayeredConfig() domain.LayeredConfig {
	strict := true
	telemetryEnabled := false
	maxRetries := 2
	var maxOverrun int64 = 0
	return domain.LayeredConfig{
		Profile: domain.ExecutionProfileHybrid,
		Telemetry: domain.TelemetryConfig{
			Enabled:    &telemetryEnabled, // Default: zero external telemetry
			Endpoint:   "",
			SampleRate: 0.0,
		},
		ZeroOverrunStrict: &strict,
		MaxRetries:        &maxRetries,
		MaxAudioOverrunMs: &maxOverrun,
		CustomSettings:    make(map[string]any),
	}
}

// ApplyProfile applies profile-specific overrides.
func ApplyProfile(cfg domain.LayeredConfig, profile domain.ExecutionProfile) domain.LayeredConfig {
	cfg.Profile = profile
	switch profile {
	case domain.ExecutionProfileLocal:
		r := 1
		cfg.MaxRetries = &r
	case domain.ExecutionProfileCloud:
		r := 3
		cfg.MaxRetries = &r
	case domain.ExecutionProfileHybrid:
		fallthrough
	default:
		cfg.Profile = domain.ExecutionProfileHybrid
		r := 2
		cfg.MaxRetries = &r
	}
	return cfg
}

func toInt(v any) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case int64:
		return int(val), true
	case float64:
		return int(val), true
	default:
		return 0, false
	}
}

func toInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case int:
		return int64(val), true
	case int64:
		return val, true
	case float64:
		return int64(val), true
	default:
		return 0, false
	}
}

// ResolveLayeredConfig resolves the configuration hierarchy:
// App Defaults -> Execution Profile -> User Settings -> Job/Run Overrides.
// Explicit false and zero overrides (e.g. telemetry.enabled=false, max_retries=0, zero_overrun_strict=false)
// are strictly preserved across layers without silently inheriting lower-layer values.
func ResolveLayeredConfig(profile domain.ExecutionProfile, userSettings *domain.LayeredConfig, runOverrides *domain.LayeredConfig) domain.LayeredConfig {
	resolved := DefaultLayeredConfig()

	if profile != "" {
		resolved = ApplyProfile(resolved, profile)
	}

	mergeLayer := func(layer *domain.LayeredConfig) {
		if layer == nil {
			return
		}
		if layer.Profile != "" {
			resolved.Profile = layer.Profile
		}
		if layer.Telemetry.Enabled != nil {
			resolved.Telemetry.Enabled = layer.Telemetry.Enabled
		}
		if layer.Telemetry.Endpoint != "" {
			resolved.Telemetry.Endpoint = layer.Telemetry.Endpoint
		}
		if layer.Telemetry.SampleRate > 0 {
			resolved.Telemetry.SampleRate = layer.Telemetry.SampleRate
		}
		if layer.ZeroOverrunStrict != nil {
			resolved.ZeroOverrunStrict = layer.ZeroOverrunStrict
		}
		if layer.MaxRetries != nil {
			resolved.MaxRetries = layer.MaxRetries
		}
		if layer.MaxAudioOverrunMs != nil {
			resolved.MaxAudioOverrunMs = layer.MaxAudioOverrunMs
		}
		if len(layer.CredentialRefs) > 0 {
			resolved.CredentialRefs = append(resolved.CredentialRefs, layer.CredentialRefs...)
		}

		for k, v := range layer.CustomSettings {
			resolved.CustomSettings[k] = v
			switch k {
			case "zero_overrun_strict":
				if b, ok := v.(bool); ok {
					resolved.ZeroOverrunStrict = &b
				}
			case "telemetry_enabled", "telemetry.enabled":
				if b, ok := v.(bool); ok {
					resolved.Telemetry.Enabled = &b
				}
			case "max_retries":
				if n, ok := toInt(v); ok {
					resolved.MaxRetries = &n
				}
			case "max_audio_overrun_ms":
				if n, ok := toInt64(v); ok {
					resolved.MaxAudioOverrunMs = &n
				}
			}
		}
	}

	mergeLayer(userSettings)
	mergeLayer(runOverrides)

	return resolved
}

// ToRunConfigSnapshot serializes the resolved configuration to a stable JSON snapshot.
func ToRunConfigSnapshot(cfg domain.LayeredConfig) (string, error) {
	bytes, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal run config snapshot: %w", err)
	}
	return string(bytes), nil
}
