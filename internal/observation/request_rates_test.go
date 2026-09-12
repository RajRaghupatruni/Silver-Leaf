package observation

import (
	"math"
	"testing"
	"time"
)

func TestDeriveRequestRates(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	metric := func(value float64) Metric {
		return Metric{Value: value, ObservedAt: now, Valid: true, Fresh: true}
	}
	tests := []struct {
		name             string
		requests         Metric
		errors           Metric
		wantRequest      float64
		wantSuccess      float64
		wantErrorRate    float64
		wantRequestValid bool
		wantSuccessValid bool
		wantErrorValid   bool
	}{
		{"normal rates", metric(100), metric(5), 100, 95, 0.05, true, true, true},
		{"zero traffic is not zero error ratio", metric(0), metric(0), 0, 0, 0, true, true, false},
		{"missing error telemetry stays unknown", metric(10), Metric{Error: "no error samples"}, 10, 0, 0, true, false, false},
		{"missing request telemetry stays unknown", Metric{Error: "no request samples"}, metric(0), 0, 0, 0, false, false, false},
		{"nonfinite error rate is rejected", metric(10), metric(math.Inf(1)), 10, 0, 0, true, false, false},
		{"error rate cannot exceed request rate", metric(2), metric(3), 2, 0, 0, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveRequestRates(tt.requests, tt.errors)
			if got.RequestRate.Valid != tt.wantRequestValid || got.SuccessfulRequestRate.Valid != tt.wantSuccessValid || got.ErrorRate.Valid != tt.wantErrorValid {
				t.Fatalf("validity got (request=%t success=%t error=%t), want (%t %t %t); values=%+v", got.RequestRate.Valid, got.SuccessfulRequestRate.Valid, got.ErrorRate.Valid, tt.wantRequestValid, tt.wantSuccessValid, tt.wantErrorValid, got)
			}
			if got.RequestRate.Valid && got.RequestRate.Value != tt.wantRequest {
				t.Errorf("request rate = %v, want %v", got.RequestRate.Value, tt.wantRequest)
			}
			if got.SuccessfulRequestRate.Valid && got.SuccessfulRequestRate.Value != tt.wantSuccess {
				t.Errorf("successful request rate = %v, want %v", got.SuccessfulRequestRate.Value, tt.wantSuccess)
			}
			if got.ErrorRate.Valid && got.ErrorRate.Value != tt.wantErrorRate {
				t.Errorf("error rate = %v, want %v", got.ErrorRate.Value, tt.wantErrorRate)
			}
			for _, derived := range []Metric{got.SuccessfulRequestRate, got.ErrorRate} {
				if derived.Valid && (math.IsNaN(derived.Value) || math.IsInf(derived.Value, 0)) {
					t.Fatalf("valid derived metric is non-finite: %+v", derived)
				}
			}
		})
	}
}

func TestDeriveRequestRatesRejectsStaleTelemetry(t *testing.T) {
	now := time.Now()
	got := DeriveRequestRates(
		Metric{Value: 5, ObservedAt: now, Valid: true, Fresh: true},
		Metric{Value: 1, ObservedAt: now.Add(-time.Hour), Valid: true, Fresh: false},
	)
	if got.SuccessfulRequestRate.Valid || got.ErrorRate.Valid {
		t.Fatalf("stale error telemetry was accepted: %+v", got)
	}
}
