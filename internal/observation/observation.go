package observation

import (
	"math"
	"time"
)

// Metric is the controller's typed view of one Prometheus result. Raw API
// responses remain inside the Prometheus client and never enter policy code.
type Metric struct {
	Value      float64
	ObservedAt time.Time
	Valid      bool
	Fresh      bool
	Error      string
}

// RequestRates contains total and successful request throughput plus the error ratio.
// ErrorRequestRate is input-only: it is the rate of the application's error counter.
type RequestRates struct {
	RequestRate           Metric
	SuccessfulRequestRate Metric
	ErrorRate             Metric
}

// DeriveRequestRates derives success throughput and error ratio from Prometheus counter rates.
// A zero denominator leaves ErrorRate invalid rather than interpreting no traffic as zero errors.
func DeriveRequestRates(requestRate, errorRequestRate Metric) RequestRates {
	result := RequestRates{RequestRate: validateRate(requestRate, "request rate")}
	if !rateUsable(result.RequestRate) {
		result.SuccessfulRequestRate = invalidRate("successful request rate requires a valid request rate")
		result.ErrorRate = invalidRate("error rate requires valid request and error rates")
		return result
	}

	validatedErrors := validateRate(errorRequestRate, "error request rate")
	if !rateUsable(validatedErrors) {
		result.SuccessfulRequestRate = invalidRate("successful request rate requires valid error telemetry")
		result.ErrorRate = invalidRate("error rate requires valid request and error rates")
		return result
	}

	observedAt := result.RequestRate.ObservedAt
	if validatedErrors.ObservedAt.Before(observedAt) {
		observedAt = validatedErrors.ObservedAt
	}
	if validatedErrors.Value > result.RequestRate.Value {
		result.SuccessfulRequestRate = invalidRate("error request rate exceeds total request rate")
		result.ErrorRate = invalidRate("error request rate exceeds total request rate")
		return result
	}
	result.SuccessfulRequestRate = Metric{
		Value: result.RequestRate.Value - validatedErrors.Value, ObservedAt: observedAt, Valid: true, Fresh: true,
	}
	if result.RequestRate.Value == 0 {
		result.ErrorRate = invalidRate("error rate is undefined when request rate is zero")
		return result
	}
	result.ErrorRate = Metric{
		Value: validatedErrors.Value / result.RequestRate.Value, ObservedAt: observedAt, Valid: true, Fresh: true,
	}
	return result
}

func validateRate(metric Metric, name string) Metric {
	if !metric.Valid {
		if metric.Error == "" {
			metric.Error = name + " is unavailable"
		}
		return metric
	}
	if !metric.Fresh {
		metric.Valid = false
		if metric.Error == "" {
			metric.Error = name + " is stale"
		}
		return metric
	}
	if metric.ObservedAt.IsZero() {
		return invalidRate(name + " timestamp is missing")
	}
	if math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) || metric.Value < 0 {
		return invalidRate(name + " must be finite and nonnegative")
	}
	return metric
}

func rateUsable(metric Metric) bool {
	return metric.Valid && metric.Fresh && !metric.ObservedAt.IsZero() && !math.IsNaN(metric.Value) && !math.IsInf(metric.Value, 0) && metric.Value >= 0
}

func invalidRate(reason string) Metric { return Metric{Error: reason} }

type TargetObservation struct {
	Name                 string
	CurrentReplicas      int32
	P95Latency           Metric
	Utilization          Metric
	UtilizationThreshold float64
	RequestRates         RequestRates
}

type DependencyObservation struct {
	Name                 string
	DependsOn            string
	CurrentReplicas      int32
	P95Latency           Metric
	Utilization          Metric
	LatencyThreshold     float64
	UtilizationThreshold float64
	Scalable             bool
	MinReplicas          int32
	MaxReplicas          int32
	RequestRates         RequestRates
}

type Snapshot struct {
	SLOTargetP95Milliseconds float64
	Target                   TargetObservation
	Dependencies             []DependencyObservation
}
