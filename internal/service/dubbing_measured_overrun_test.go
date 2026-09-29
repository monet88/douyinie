package service

import (
	"testing"

	"github.com/monet88/douyinie/internal/provider"
)

// Issue #154 / #150 R2: the fit controller reads exactly one declaration for "this lane cannot
// honor a rate change" (ttsFitCapabilities -> FitEvaluationInput.FixedRateVoice). Pinning the
// production-registered VieNeu lane here keeps the routing half of that contract wired to the
// lane that actually declares fixed-rate.
func TestVieNeuProductionLaneIsNeverOfferedNativeSpeed(t *testing.T) {
	reg, err := provider.NewProductionSpeechRegistry(false)
	if err != nil {
		t.Fatalf("NewProductionSpeechRegistry: %v", err)
	}
	vieneu, ok := reg.Get(provider.VieNeuProviderID)
	if !ok {
		t.Fatalf("missing production lane %s", provider.VieNeuProviderID)
	}
	if !ttsFitCapabilities(vieneu) {
		t.Fatalf("VieNeu must reach the fit controller as fixed-rate, got features %+v", vieneu.Capability().Features)
	}
}

// Issue #154 review finding: the spoken adapter's OverrunMs is documented as the measured overrun
// above PlaybackAllowanceMs, so it must be measured against that window and nothing else. The
// frame-exact REWRITE path reaches the adapter with a floored millisecond probe that still fits
// the window while the resampled waveform does not; substituting the fit's DurationDeltaMs there
// would report an overrun against the reserve-reduced usable slot, a budget the candidate never
// exceeded. The value is therefore taken from the accepted window itself, frame evidence included.
func TestMeasuredOverrunAboveAllowanceMs(t *testing.T) {
	const (
		startMs, playbackEndMs = 0, 1000 // accepted playback allowance: 1000ms
		outputSampleRate       = 48000
	)
	for _, tc := range []struct {
		name               string
		probedMs           int64
		measuredFrames     int64
		measuredSampleRate int
		outputRate         int
		want               int64
	}{
		{
			name:       "probe_overrun_is_reported_verbatim",
			probedMs:   1400,
			outputRate: outputSampleRate,
			want:       400,
		},
		{
			name:       "probe_inside_window_without_frame_evidence_reports_no_overrun",
			probedMs:   1000,
			outputRate: outputSampleRate,
			want:       0,
		},
		{
			// 16001 frames at 16kHz resample to round(16001*48000/16000) = 48003 output frames
			// against a window of exactly 48000: 3 excess frames = 0.0625ms, rounded up to 1ms.
			name:               "frame_excess_below_one_probe_millisecond_is_reported",
			probedMs:           1000,
			measuredFrames:     16001,
			measuredSampleRate: 16000,
			outputRate:         outputSampleRate,
			want:               1,
		},
		{
			name:               "frame_evidence_inside_window_reports_no_overrun",
			probedMs:           1000,
			measuredFrames:     16000,
			measuredSampleRate: 16000,
			outputRate:         outputSampleRate,
			want:               0,
		},
		{
			name:               "unknown_output_rate_cannot_prove_a_frame_overrun",
			probedMs:           1000,
			measuredFrames:     16001,
			measuredSampleRate: 16000,
			outputRate:         0,
			want:               0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := measuredOverrunAboveAllowanceMs(tc.probedMs, startMs, playbackEndMs,
				tc.measuredFrames, tc.measuredSampleRate, tc.outputRate)
			if got != tc.want {
				t.Fatalf("overrun above allowance = %dms, want %dms", got, tc.want)
			}
		})
	}
}
