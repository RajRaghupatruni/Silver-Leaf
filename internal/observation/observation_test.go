package observation

import (
	"math"
	"testing"
	"time"
)

func TestMeanSlotOccupancyUsesTrafficBearingReplicas(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	aggregate := Metric{Value: 40, ObservedAt: now, Valid: true, Fresh: true}
	tests := []struct {
		name      string
		ready     int32
		effective float64
		want      float64
		valid     bool
	}{
		{name: "two active replicas", ready: 2, effective: 2, want: 20, valid: true},
		{name: "idle Ready pod does not dilute occupancy", ready: 3, effective: 2, want: 20, valid: true},
		{name: "three active replicas spread same aggregate", ready: 3, effective: 3, want: 40.0 / 3, valid: true},
		{name: "zero effective replicas does not divide", ready: 3, effective: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serving := Metric{Value: test.effective, ObservedAt: now, Valid: true, Fresh: true}
			got := MeanSlotOccupancy(aggregate, serving, test.ready)
			if got.Valid != test.valid {
				t.Fatalf("valid=%t, want %t (error %q)", got.Valid, test.valid, got.Error)
			}
			if test.valid && math.Abs(got.Value-test.want) > 1e-9 {
				t.Fatalf("mean occupancy %.4f, want %.4f", got.Value, test.want)
			}
			if test.valid && !got.ObservedAt.Equal(now) {
				t.Fatalf("normalization changed observation time: %s", got.ObservedAt)
			}
		})
	}
}

func TestEffectiveReplicaCountRejectsUnsafeTelemetry(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name   string
		metric Metric
		ready  int32
		want   int32
		valid  bool
	}{
		{name: "valid zero", metric: Metric{Value: 0, ObservedAt: now, Valid: true, Fresh: true}, ready: 3, want: 0, valid: true},
		{name: "valid integral count", metric: Metric{Value: 2, ObservedAt: now, Valid: true, Fresh: true}, ready: 3, want: 2, valid: true},
		{name: "fractional", metric: Metric{Value: 1.5, ObservedAt: now, Valid: true, Fresh: true}, ready: 3},
		{name: "more than Ready", metric: Metric{Value: 4, ObservedAt: now, Valid: true, Fresh: true}, ready: 3},
		{name: "stale", metric: Metric{Value: 2, ObservedAt: now, Valid: true, Fresh: false}, ready: 3},
		{name: "non-finite", metric: Metric{Value: math.NaN(), ObservedAt: now, Valid: true, Fresh: true}, ready: 3},
		{name: "missing", metric: Metric{}, ready: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := EffectiveReplicaCount(test.metric, test.ready)
			if (err == nil) != test.valid {
				t.Fatalf("count=%d err=%v, valid=%t", got, err, test.valid)
			}
			if err == nil && got != test.want {
				t.Fatalf("count=%d, want %d", got, test.want)
			}
		})
	}
}

func TestMeanSlotOccupancyRejectsInvalidAggregate(t *testing.T) {
	now := time.Now().UTC()
	serving := Metric{Value: 2, ObservedAt: now, Valid: true, Fresh: true}
	tests := []struct {
		name     string
		metric   Metric
		contains string
	}{
		{name: "missing", metric: Metric{}, contains: "unavailable"},
		{name: "stale", metric: Metric{Value: 1, ObservedAt: now, Valid: true, Fresh: false}, contains: "stale"},
		{name: "nan", metric: Metric{Value: math.NaN(), ObservedAt: now, Valid: true, Fresh: true}, contains: "finite"},
		{name: "infinite", metric: Metric{Value: math.Inf(1), ObservedAt: now, Valid: true, Fresh: true}, contains: "finite"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := MeanSlotOccupancy(test.metric, serving, 2)
			if got.Valid || got.Error == "" || !containsText(got.Error, test.contains) {
				t.Fatalf("expected invalid metric with %q error, got %+v", test.contains, got)
			}
		})
	}
}

func containsText(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
