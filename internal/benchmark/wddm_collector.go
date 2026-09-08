package benchmark

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WDDMGPUCollector collects device-wide GPU VRAM metrics on Windows WDDM
// via periodic sampling using nvidia-smi.
// Invariant (Issue #59 / #72): On Windows WDDM, per-process VRAM is not available.
// Telemetry records device-wide baseline, peak, and peak-above-baseline VRAM
// bound to stage timestamps. Does not claim unavailable per-process VRAM precision.
type WDDMGPUCollector struct {
	mu           sync.RWMutex
	nvidiaSmiBin string
	cadence      time.Duration
	samples      []wddmSample
	ctx          context.Context
	cancel       context.CancelFunc
	running      bool
	queryFn      func() (uint64, error) // injectable for deterministic testing
}

type wddmSample struct {
	timestamp time.Time
	usedVRAM  uint64 // bytes
}

// NewWDDMGPUCollector creates a new Windows WDDM GPU collector with a specified sampling cadence (e.g. 50ms - 200ms).
func NewWDDMGPUCollector(cadence time.Duration) *WDDMGPUCollector {
	if cadence <= 0 {
		cadence = 100 * time.Millisecond
	}
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		bin = "nvidia-smi"
	}
	c := &WDDMGPUCollector{
		nvidiaSmiBin: bin,
		cadence:      cadence,
	}
	c.queryFn = c.queryNvidiaSMI
	return c
}

// queryNvidiaSMI queries nvidia-smi for current used memory in bytes.
func (c *WDDMGPUCollector) queryNvidiaSMI() (uint64, error) {
	cmd := exec.Command(c.nvidiaSmiBin, "--query-gpu=memory.used", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi query failed: %w", err)
	}
	str := strings.TrimSpace(string(out))
	// Parse MiB and convert to bytes
	valMiB, err := strconv.ParseUint(str, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse nvidia-smi output %q: %w", str, err)
	}
	return valMiB * 1024 * 1024, nil
}

// Start initiates background periodic sampling.
func (c *WDDMGPUCollector) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return nil
	}

	// Initial probe to verify query works
	initial, err := c.queryFn()
	if err != nil {
		return fmt.Errorf("gpu collector start probe failed: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	c.cancel = cancel
	c.running = true
	c.samples = []wddmSample{
		{timestamp: time.Now().UTC(), usedVRAM: initial},
	}

	go c.sampleLoop(ctx)
	return nil
}

// Stop terminates background sampling.
func (c *WDDMGPUCollector) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.running {
		return
	}
	c.cancel()
	c.running = false
}

func (c *WDDMGPUCollector) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			used, err := c.queryFn()
			if err == nil {
				c.mu.Lock()
				c.samples = append(c.samples, wddmSample{
					timestamp: time.Now().UTC(),
					usedVRAM:  used,
				})
				// Bound memory to last 5000 samples (~8-10 minutes at 100ms)
				if len(c.samples) > 5000 {
					c.samples = c.samples[len(c.samples)-4000:]
				}
				c.mu.Unlock()
			}
		}
	}
}

// RecordSample allows manually recording an authentic sample (e.g. for testing).
func (c *WDDMGPUCollector) RecordSample(ts time.Time, usedBytes uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, wddmSample{
		timestamp: ts.UTC(),
		usedVRAM:  usedBytes,
	})
}

// SampleStage implements ResourceSampler.
// It finds all samples bounded between startedAt and startedAt + elapsedMs,
// computing authentic baseline (start/closest), peak, and peak-above-baseline VRAM.
func (c *WDDMGPUCollector) SampleStage(stage string, startedAt time.Time, elapsedMs int64) (ResourceTelemetrySample, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if len(c.samples) == 0 {
		return ResourceTelemetrySample{}, false
	}

	stageStart := startedAt.UTC()
	stageEnd := stageStart.Add(time.Duration(elapsedMs) * time.Millisecond)

	// Filter samples within [stageStart - cadence, stageEnd + cadence]
	var inWindow []wddmSample
	cadence := c.cadence
	windowStart := stageStart.Add(-cadence)
	windowEnd := stageEnd.Add(cadence)

	var closestPrior *wddmSample
	for _, s := range c.samples {
		if s.timestamp.Before(stageStart) {
			closestPrior = &s
		}
		if (s.timestamp.Equal(windowStart) || s.timestamp.After(windowStart)) &&
			(s.timestamp.Equal(windowEnd) || s.timestamp.Before(windowEnd)) {
			inWindow = append(inWindow, s)
		}
	}

	if len(inWindow) == 0 {
		if closestPrior != nil {
			inWindow = []wddmSample{*closestPrior}
		} else {
			return ResourceTelemetrySample{}, false
		}
	}

	baseline := inWindow[0].usedVRAM
	peak := baseline
	for _, s := range inWindow {
		if s.usedVRAM > peak {
			peak = s.usedVRAM
		}
	}

	sample := ComputeResourceSample(stage, stageStart, baseline, peak, elapsedMs, 0)
	return sample, true
}
