package forecast

import (
	"math"
	"testing"
	"time"
)

func TestLinearForecastAcceptsCleanRisingTrend(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	samples := linearSamples(now.Add(-time.Minute), 5, 15*time.Second, 10, 2)
	got := LinearForecast(samples, now, 12*time.Second)
	if got.Quality != QualityHigh || !got.HasRequestRate || !got.HasTrend || !got.HasFit {
		t.Fatalf("expected a high-quality forecast, got %+v", got)
	}
	if math.Abs(got.Slope-2) > 1e-9 || math.Abs(got.RequestRate-154) > 1e-9 || got.R2 < 0.999 {
		t.Fatalf("unexpected regression result: %+v", got)
	}
}

func TestLinearForecastUsesRecentWindowAfterFlatPrefix(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	latest := now.Add(-30 * time.Second)
	start := latest.Add(-270 * time.Second)
	samples := make([]Sample, MaxSamples-1)
	for i := range samples {
		requestRate := 200.0
		if i >= 10 {
			requestRate += float64(i-10) * 30
		}
		samples[i] = Sample{Timestamp: start.Add(time.Duration(i) * 15 * time.Second), RequestRate: requestRate}
	}

	got := LinearForecast(samples, now, 37*time.Second)
	if got.Quality != QualityHigh || !got.HasFit {
		t.Fatalf("expected recent ramp to qualify HIGH despite retained flat prefix, got %+v", got)
	}
	if got.Samples != 9 || got.Span != TrendFitWindow || math.Abs(got.Slope-2) > 1e-9 {
		t.Fatalf("fit metadata=(samples %d, span %s, slope %.3f); want (9, %s, 2)", got.Samples, got.Span, got.Slope, TrendFitWindow)
	}
}

func TestLinearForecastRecentNoisyTrendDoesNotBecomeHigh(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	samples := make([]Sample, 0, MaxSamples)
	for i := 0; i < 11; i++ {
		samples = append(samples, Sample{Timestamp: now.Add(-285*time.Second + time.Duration(i)*15*time.Second), RequestRate: 100})
	}
	deviations := []float64{0, 80, -60, 60, -80, 80, -60, 60, 0}
	for i, deviation := range deviations {
		samples = append(samples, Sample{
			Timestamp:   now.Add(-120*time.Second + time.Duration(i)*15*time.Second),
			RequestRate: 200 + float64(i)*30 + deviation,
		})
	}

	got := LinearForecast(samples, now, 15*time.Second)
	if got.Quality == QualityHigh || got.Samples != 9 || got.Span != TrendFitWindow {
		t.Fatalf("recent noisy trend should remain non-HIGH using 9 points over %s, got %+v", TrendFitWindow, got)
	}
}

func TestLinearForecastAppliesSampleAndSpanGatesToRecentWindow(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		samples     []Sample
		horizon     time.Duration
		wantSamples int
		wantSpan    time.Duration
		wantReason  string
	}{
		{
			name: "fewer than five recent samples",
			samples: append(
				linearSamples(now.Add(-290*time.Second), 16, 10*time.Second, 100, 1),
				linearSamples(now.Add(-45*time.Second), 4, 15*time.Second, 200, 1)...,
			),
			horizon: 10 * time.Second, wantSamples: 4, wantSpan: 45 * time.Second, wantReason: "at least 5",
		},
		{
			name: "recent span below one minute",
			samples: append(
				linearSamples(now.Add(-290*time.Second), 15, 10*time.Second, 100, 1),
				linearSamples(now.Add(-40*time.Second), 5, 10*time.Second, 200, 1)...,
			),
			horizon: 10 * time.Second, wantSamples: 5, wantSpan: 40 * time.Second, wantReason: "need at least 1m0s",
		},
		{
			name: "horizon exceeds recent span despite older retained data",
			samples: append(
				linearSamples(now.Add(-290*time.Second), 15, 10*time.Second, 100, 1),
				linearSamples(now.Add(-60*time.Second), 5, 15*time.Second, 200, 1)...,
			),
			horizon: 61 * time.Second, wantSamples: 5, wantSpan: time.Minute, wantReason: "horizon exceeds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := LinearForecast(test.samples, now, test.horizon)
			if got.Quality == QualityHigh || !contains(got.Reason, test.wantReason) {
				t.Fatalf("expected recent-window rejection containing %q, got %+v", test.wantReason, got)
			}
			if got.Samples != test.wantSamples || got.Span != test.wantSpan {
				t.Fatalf("reported fit metadata=(samples %d, span %s); want (%d, %s)", got.Samples, got.Span, test.wantSamples, test.wantSpan)
			}
		})
	}
}

func TestLinearForecastRejectsInsufficientFlatFallingAndNoisyHistory(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		samples []Sample
		want    string
	}{
		{name: "insufficient history", samples: linearSamples(now.Add(-time.Minute), 4, 15*time.Second, 1, 1), want: "at least 5"},
		{name: "flat traffic", samples: linearSamples(now.Add(-time.Minute), 5, 15*time.Second, 20, 0), want: "flat"},
		{name: "declining traffic", samples: linearSamples(now.Add(-time.Minute), 5, 15*time.Second, 80, -1), want: "falling"},
		{name: "poor fit", samples: noisySamples(now.Add(-time.Minute)), want: "not HIGH quality"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := LinearForecast(test.samples, now, 15*time.Second)
			if got.Quality == QualityHigh || got.Reason == "" || !contains(got.Reason, test.want) {
				t.Fatalf("expected rejected forecast containing %q, got %+v", test.want, got)
			}
		})
	}
}

func TestLinearForecastRejectsStaleNonFiniteAndGappedSamples(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		samples []Sample
		want    string
	}{
		{name: "stale", samples: linearSamples(now.Add(-2*time.Minute), 5, 15*time.Second, 1, 1), want: "stale"},
		{name: "non-finite", samples: func() []Sample {
			s := linearSamples(now.Add(-time.Minute), 5, 15*time.Second, 1, 1)
			s[2].RequestRate = math.NaN()
			return s
		}(), want: "non-finite"},
		{name: "sample gap", samples: []Sample{{now.Add(-90 * time.Second), 1}, {now.Add(-75 * time.Second), 2}, {now.Add(-60 * time.Second), 3}, {now.Add(-5 * time.Second), 4}, {now, 5}}, want: "gap"},
		{name: "horizon beyond observed trend", samples: linearSamples(now.Add(-time.Minute), 5, 15*time.Second, 1, 1), want: "horizon exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			horizon := 10 * time.Second
			if test.name == "horizon beyond observed trend" {
				horizon = 61 * time.Second
			}
			got := LinearForecast(test.samples, now, horizon)
			if got.Quality == QualityHigh || !contains(got.Reason, test.want) {
				t.Fatalf("expected forecast rejection containing %q, got %+v", test.want, got)
			}
		})
	}
}

func TestAppendSampleBoundsHistoryAndDropsOldSamples(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	history := make([]Sample, 0, MaxSamples+1)
	for i := 0; i < MaxSamples+4; i++ {
		timestamp := now.Add(-time.Duration(MaxSamples+3-i) * 10 * time.Second)
		history = AppendSample(history, Sample{Timestamp: timestamp, RequestRate: float64(i + 1)}, now)
	}
	if len(history) != MaxSamples {
		t.Fatalf("history length %d; want %d", len(history), MaxSamples)
	}
	old := AppendSample(history, Sample{Timestamp: now.Add(-HistoryWindow - time.Second), RequestRate: 100}, now)
	if len(old) != MaxSamples {
		t.Fatalf("old sample unexpectedly changed history length: %d", len(old))
	}
}

func TestSafePerReplicaCapacityExtrapolatesToPhysicalLimitThenAppliesMargin(t *testing.T) {
	now := time.Now().UTC()
	samples := []CapacitySample{
		{Timestamp: now.Add(-30 * time.Second), RPSPerWorkUnit: 10},
		{Timestamp: now.Add(-20 * time.Second), RPSPerWorkUnit: 12},
		{Timestamp: now.Add(-10 * time.Second), RPSPerWorkUnit: 11},
	}
	got, ok := SafePerReplicaCapacity(samples, 20, 0.75)
	if !ok || got.ConservativeRPSPerWorkUnit != 10 || got.EstimatedPerReplicaCapacity != 200 || got.SafePerReplicaCapacity != 150 {
		t.Fatalf("capacity estimate=(%+v,%t); want efficiency=10, physical=200, safe=150", got, ok)
	}
	if _, ok := SafePerReplicaCapacity(samples[:2], 20, 0.75); ok {
		t.Fatal("accepted fewer than three capacity observations")
	}
	if _, ok := SafePerReplicaCapacity(samples, 20, 1.1); ok {
		t.Fatal("accepted an invalid safety margin")
	}
	if _, ok := SafePerReplicaCapacity(samples, 0, 0.75); ok {
		t.Fatal("accepted a non-positive physical concurrency limit")
	}
	if _, ok := SafePerReplicaCapacity([]CapacitySample{
		{Timestamp: now.Add(-30 * time.Second), RPSPerWorkUnit: 10},
		{Timestamp: now.Add(-20 * time.Second), RPSPerWorkUnit: math.NaN()},
		{Timestamp: now.Add(-10 * time.Second), RPSPerWorkUnit: 11},
	}, 20, 0.75); ok {
		t.Fatal("accepted non-finite or unusable throughput efficiency")
	}
}

func TestSafePerReplicaCapacityUsesCalibrationFormula(t *testing.T) {
	now := time.Now().UTC()
	efficiency := 190.0 / 13.8
	samples := []CapacitySample{
		{Timestamp: now.Add(-30 * time.Second), RPSPerWorkUnit: efficiency},
		{Timestamp: now.Add(-20 * time.Second), RPSPerWorkUnit: efficiency},
		{Timestamp: now.Add(-10 * time.Second), RPSPerWorkUnit: efficiency},
	}
	got, ok := SafePerReplicaCapacity(samples, 20, 0.75)
	if !ok {
		t.Fatal("calibration samples did not produce a capacity estimate")
	}
	wantEstimated := (190.0 / 13.8) * 20
	wantSafe := wantEstimated * 0.75
	if math.Abs(got.EstimatedPerReplicaCapacity-wantEstimated) > 1e-9 || math.Abs(got.SafePerReplicaCapacity-wantSafe) > 1e-9 {
		t.Fatalf("estimate=%+v, want estimated %.4f and safe %.4f", got, wantEstimated, wantSafe)
	}
}

func linearSamples(start time.Time, count int, step time.Duration, base, slopePerSecond float64) []Sample {
	samples := make([]Sample, count)
	for i := range samples {
		samples[i] = Sample{Timestamp: start.Add(time.Duration(i) * step), RequestRate: base + slopePerSecond*float64(i)*step.Seconds()}
	}
	return samples
}

func noisySamples(start time.Time) []Sample {
	values := []float64{10, 48, 14, 58, 19}
	samples := make([]Sample, len(values))
	for i, value := range values {
		samples[i] = Sample{Timestamp: start.Add(time.Duration(i) * 15 * time.Second), RequestRate: value}
	}
	return samples
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
