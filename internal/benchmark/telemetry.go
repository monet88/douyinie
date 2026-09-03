package benchmark

import (
	"time"
)

// ResourceSampler is an optional extension point for authentic resource telemetry collectors.
// Invariant (#65 / #72): Telemetry must remain absent unless an authentic measurement collector
// (such as the Windows WDDM GPU collector owned by #72) supplies real samples.
// The runner must not fabricate dummy zero samples.
type ResourceSampler interface {
	SampleStage(stage string, startedAt time.Time, elapsedMs int64) (ResourceTelemetrySample, bool)
}

// ComputeResourceSample calculates a ResourceTelemetrySample from authentic measurements.
// Invariant (Issue #59 / #63): On Windows WDDM, per-process VRAM is not available.
// Telemetry records device-wide baseline, peak, and peak-above-baseline VRAM bound to stage timestamps.
func ComputeResourceSample(stage string, timestamp time.Time, baselineVRAM, peakVRAM uint64, elapsedMs, mediaDurationMs int64) ResourceTelemetrySample {
	var peakAboveBaseline uint64
	if peakVRAM > baselineVRAM {
		peakAboveBaseline = peakVRAM - baselineVRAM
	}

	var rtf float64
	if mediaDurationMs > 0 {
		rtf = float64(elapsedMs) / float64(mediaDurationMs)
	}

	return ResourceTelemetrySample{
		Stage:                        stage,
		Timestamp:                    timestamp,
		DeviceBaselineVRAMBytes:      baselineVRAM,
		DevicePeakVRAMBytes:          peakVRAM,
		DevicePeakAboveBaselineBytes: peakAboveBaseline,
		ElapsedMs:                    elapsedMs,
		RTF:                          rtf,
	}
}
