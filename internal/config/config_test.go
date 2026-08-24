package config_test

import (
	"testing"

	"github.com/monet88/douyinie/internal/config"
	"github.com/monet88/douyinie/internal/domain"
)

func TestResolveLayeredConfig_DefaultsAndHierarchy(t *testing.T) {
	// 1. Defaults
	def := config.DefaultLayeredConfig()
	if def.Profile != domain.ExecutionProfileHybrid {
		t.Errorf("expected default profile Hybrid, got %s", def.Profile)
	}
	if def.Telemetry.IsEnabled() {
		t.Errorf("expected telemetry disabled by default, got true")
	}
	if def.GetMaxRetries() != 2 {
		t.Errorf("expected default max_retries 2, got %d", def.GetMaxRetries())
	}

	// 2. Layering: Defaults -> Profile -> User -> Run
	maxOverrun := int64(200)
	userSettings := &domain.LayeredConfig{
		MaxAudioOverrunMs: &maxOverrun,
		CustomSettings:    map[string]any{"user_key": "user_val"},
	}
	runOverrides := &domain.LayeredConfig{
		Profile:        domain.ExecutionProfileLocal,
		CustomSettings: map[string]any{"run_key": "run_val"},
	}

	resolved := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userSettings, runOverrides)
	if resolved.Profile != domain.ExecutionProfileLocal {
		t.Errorf("expected run override profile 'local', got %s", resolved.Profile)
	}
	if resolved.GetMaxAudioOverrunMs() != 200 {
		t.Errorf("expected user setting 200ms, got %d", resolved.GetMaxAudioOverrunMs())
	}
	if resolved.CustomSettings["user_key"] != "user_val" || resolved.CustomSettings["run_key"] != "run_val" {
		t.Errorf("custom settings not properly merged: %+v", resolved.CustomSettings)
	}
	if resolved.Telemetry.IsEnabled() {
		t.Errorf("telemetry should remain disabled")
	}
	if !resolved.IsZeroOverrunStrict() {
		t.Errorf("expected default ZeroOverrunStrict = true")
	}

	// 3. Explicit boolean override: zero_overrun_strict = false in user settings
	falseVal := false
	userWithFalse := &domain.LayeredConfig{
		ZeroOverrunStrict: &falseVal,
	}
	resFalse := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userWithFalse, nil)
	if resFalse.IsZeroOverrunStrict() {
		t.Errorf("expected explicit ZeroOverrunStrict=false to be preserved, got true")
	}

	// 4. Explicit boolean override via CustomSettings map
	userWithMapOverride := &domain.LayeredConfig{
		CustomSettings: map[string]any{"zero_overrun_strict": false},
	}
	resMapFalse := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userWithMapOverride, nil)
	if resMapFalse.IsZeroOverrunStrict() {
		t.Errorf("expected custom_settings zero_overrun_strict=false to be preserved, got true")
	}

	// 5. Run override overriding user setting
	trueVal := true
	runWithTrue := &domain.LayeredConfig{
		ZeroOverrunStrict: &trueVal,
	}
	resOverride := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userWithFalse, runWithTrue)
	if !resOverride.IsZeroOverrunStrict() {
		t.Errorf("expected run override ZeroOverrunStrict=true to take precedence over user setting false")
	}

	// 6. Explicit false override for telemetry: user sets true, run overrides with explicit false
	userTelemetryTrue := &domain.LayeredConfig{
		Telemetry: domain.TelemetryConfig{
			Enabled: &trueVal,
		},
	}
	runTelemetryFalse := &domain.LayeredConfig{
		Telemetry: domain.TelemetryConfig{
			Enabled: &falseVal,
		},
	}
	resTelOverride := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userTelemetryTrue, runTelemetryFalse)
	if resTelOverride.Telemetry.IsEnabled() {
		t.Errorf("expected explicit telemetry.enabled=false in runOverrides to override user true, got true")
	}

	// 7. Explicit zero override for max_retries: profile sets 3 (cloud), user sets max_retries = 0
	zeroRetries := 0
	userZeroRetries := &domain.LayeredConfig{
		MaxRetries: &zeroRetries,
	}
	resZeroRetries := config.ResolveLayeredConfig(domain.ExecutionProfileCloud, userZeroRetries, nil)
	if resZeroRetries.GetMaxRetries() != 0 {
		t.Errorf("expected explicit max_retries=0 to be preserved, got %d", resZeroRetries.GetMaxRetries())
	}

	// 8. Explicit zero override for max_retries in runOverrides overriding user retries
	two := 2
	userTwoRetries := &domain.LayeredConfig{
		MaxRetries: &two,
	}
	runZeroRetries := &domain.LayeredConfig{
		MaxRetries: &zeroRetries,
	}
	resRunZeroRetries := config.ResolveLayeredConfig(domain.ExecutionProfileHybrid, userTwoRetries, runZeroRetries)
	if resRunZeroRetries.GetMaxRetries() != 0 {
		t.Errorf("expected runOverrides max_retries=0 to override user setting, got %d", resRunZeroRetries.GetMaxRetries())
	}

	// 9. CustomSettings map overrides for telemetry.enabled=false and max_retries=0
	customMapOverride := &domain.LayeredConfig{
		CustomSettings: map[string]any{
			"telemetry.enabled": false,
			"max_retries":       0,
		},
	}
	resCustomMap := config.ResolveLayeredConfig(domain.ExecutionProfileCloud, userTelemetryTrue, customMapOverride)
	if resCustomMap.Telemetry.IsEnabled() {
		t.Errorf("expected custom_settings telemetry.enabled=false to override user true, got true")
	}
	if resCustomMap.GetMaxRetries() != 0 {
		t.Errorf("expected custom_settings max_retries=0 to override profile cloud, got %d", resCustomMap.GetMaxRetries())
	}
}
