package forecast

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const (
	MaxSamples             = 20
	HistoryWindow          = 5 * time.Minute
	TrendFitWindow         = 120 * time.Second
	MinimumSamples         = 5
	MinimumSpan            = 60 * time.Second
	MaximumSampleAge       = 45 * time.Second
	MaximumSampleGap       = 45 * time.Second
	MinimumCapacitySamples = 3
)

type Quality string

const (
	QualityHigh   Quality = "HIGH"
	QualityMedium Quality = "MEDIUM"
	QualityLow    Quality = "LOW"
)

// Sample is one timestamped request-rate observation, in requests per second.
type Sample struct {
	Timestamp   time.Time
	RequestRate float64
}

// Result is a deterministic least-squares forecast evaluated at Horizon after the latest sample.
type Result struct {
	RequestRate    float64
	Slope          float64
	R2             float64
	NormalizedRMSE float64
	Samples        int
	Span           time.Duration
	Quality        Quality
	Reason         string
	HasTrend       bool
	HasRequestRate bool
	HasFit         bool
}

// AppendSample retains only a bounded recent history and avoids duplicate or out-of-order timestamps.
func AppendSample(history []Sample, sample Sample, now time.Time) []Sample {
	history = pruneSamples(history, now)
	if sample.Timestamp.IsZero() || sample.Timestamp.Before(now.Add(-HistoryWindow)) || sample.Timestamp.After(now.Add(30*time.Second)) || !finiteNonnegative(sample.RequestRate) {
		return history
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		if sample.Timestamp.Before(last.Timestamp) {
			return history
		}
		if sample.Timestamp.Equal(last.Timestamp) {
			history[len(history)-1] = sample
			return history
		}
	}
	history = append(history, sample)
	if len(history) > MaxSamples {
		history = append([]Sample(nil), history[len(history)-MaxSamples:]...)
	}
	return history
}

// LinearForecast computes a least-squares trend over the fixed trailing window
// ending at the latest sample, then extrapolates it by horizon. Retained history
// may be longer than the active fit window.
// A forecast is HIGH quality only with at least five points over one minute,
// a positive slope, R^2 >= .95, normalized RMSE <= .10, and fresh gap-free data.
func LinearForecast(samples []Sample, now time.Time, horizon time.Duration) Result {
	result := Result{Quality: QualityLow}
	if horizon <= 0 {
		result.Reason = "planning horizon must be positive"
		return result
	}
	if len(samples) > MaxSamples {
		result.Reason = "request-rate history exceeds the configured sample bound"
		return result
	}
	if len(samples) == 0 {
		result.Reason = fmt.Sprintf("insufficient request-rate history: need at least %d observations", MinimumSamples)
		return result
	}
	latest := samples[len(samples)-1].Timestamp
	if latest.IsZero() {
		result.Reason = "request-rate history contains a missing timestamp"
		return result
	}
	cutoff := latest.Add(-TrendFitWindow)
	fitSamples := make([]Sample, 0, len(samples))
	for _, sample := range samples {
		if !sample.Timestamp.Before(cutoff) {
			fitSamples = append(fitSamples, sample)
		}
	}
	result.Samples = len(fitSamples)
	if len(fitSamples) == 0 {
		result.Reason = fmt.Sprintf("insufficient recent request-rate history: need at least %d observations", MinimumSamples)
		return result
	}
	first := fitSamples[0].Timestamp
	last := fitSamples[len(fitSamples)-1].Timestamp
	if first.IsZero() || last.IsZero() {
		result.Reason = "request-rate history contains a missing timestamp"
		return result
	}
	result.Span = last.Sub(first)
	if len(fitSamples) < MinimumSamples {
		result.Reason = fmt.Sprintf("insufficient recent request-rate history: need at least %d observations", MinimumSamples)
		return result
	}
	if result.Span < MinimumSpan {
		result.Reason = fmt.Sprintf("recent request-rate history spans %s; need at least %s", result.Span.Round(time.Second), MinimumSpan)
		return result
	}
	if horizon > result.Span {
		result.Reason = "planning horizon exceeds the observed request-rate trend span"
		return result
	}
	if last.After(now.Add(30*time.Second)) || now.Sub(last) > MaximumSampleAge {
		result.Reason = "latest request-rate sample is stale or future-dated"
		return result
	}
	for i, sample := range fitSamples {
		if !finiteNonnegative(sample.RequestRate) || sample.Timestamp.IsZero() {
			result.Reason = "request-rate history contains a missing or non-finite observation"
			return result
		}
		if i > 0 {
			gap := sample.Timestamp.Sub(fitSamples[i-1].Timestamp)
			if gap <= 0 || gap > MaximumSampleGap {
				result.Reason = "request-rate history has a stale or non-monotonic sample gap"
				return result
			}
		}
	}

	meanX, meanY := 0.0, 0.0
	for _, sample := range fitSamples {
		meanX += sample.Timestamp.Sub(first).Seconds()
		meanY += sample.RequestRate
	}
	meanX /= float64(len(fitSamples))
	meanY /= float64(len(fitSamples))

	var covariance, varianceX, varianceY float64
	minimum, maximum := math.Inf(1), math.Inf(-1)
	for _, sample := range fitSamples {
		x := sample.Timestamp.Sub(first).Seconds()
		dx, dy := x-meanX, sample.RequestRate-meanY
		covariance += dx * dy
		varianceX += dx * dx
		varianceY += dy * dy
		minimum = math.Min(minimum, sample.RequestRate)
		maximum = math.Max(maximum, sample.RequestRate)
	}
	if varianceX == 0 || varianceY == 0 || maximum == minimum {
		result.Reason = "request-rate trend is flat and has no predictive signal"
		return result
	}
	slope := covariance / varianceX
	intercept := meanY - slope*meanX
	if !finite(slope) || !finite(intercept) {
		result.Reason = "request-rate trend is non-finite"
		return result
	}
	result.Slope = slope
	result.HasTrend = true
	if slope <= 0 {
		result.Reason = "request-rate trend is flat or falling"
		return result
	}

	var residualSquares float64
	for _, sample := range fitSamples {
		x := sample.Timestamp.Sub(first).Seconds()
		residual := sample.RequestRate - (intercept + slope*x)
		residualSquares += residual * residual
	}
	result.R2 = 1 - residualSquares/varianceY
	if result.R2 < 0 {
		result.R2 = 0
	}
	result.NormalizedRMSE = math.Sqrt(residualSquares/float64(len(fitSamples))) / (maximum - minimum)
	result.HasFit = finite(result.R2) && finite(result.NormalizedRMSE)
	if !result.HasFit {
		result.Reason = "request-rate fit quality is non-finite"
		return result
	}

	forecastAt := last.Sub(first).Seconds() + horizon.Seconds()
	result.RequestRate = intercept + slope*forecastAt
	if !finiteNonnegative(result.RequestRate) {
		result.Reason = "forecast request rate is non-finite or negative"
		return result
	}
	result.HasRequestRate = true
	switch {
	case result.R2 >= 0.95 && result.NormalizedRMSE <= 0.10:
		result.Quality = QualityHigh
	case result.R2 >= 0.85 && result.NormalizedRMSE <= 0.20:
		result.Quality = QualityMedium
	default:
		result.Quality = QualityLow
	}
	if result.Quality != QualityHigh {
		result.Reason = "forecast fit is not HIGH quality (requires R^2 >= 0.95 and normalized RMSE <= 0.10)"
		return result
	}
	result.Reason = "high-quality positive request-rate trend"
	return result
}

type CapacitySample struct {
	Timestamp time.Time
	// RPSPerWorkUnit is successful requests/second divided by aggregate local-work rate.
	RPSPerWorkUnit float64
}

func AppendCapacitySample(history []CapacitySample, sample CapacitySample, now time.Time) []CapacitySample {
	cutoff := now.Add(-HistoryWindow)
	kept := history[:0]
	for _, old := range history {
		if !old.Timestamp.Before(cutoff) && !old.Timestamp.After(now.Add(30*time.Second)) && finite(old.RPSPerWorkUnit) && old.RPSPerWorkUnit > 0 {
			kept = append(kept, old)
		}
	}
	history = kept
	if sample.Timestamp.IsZero() || sample.Timestamp.Before(cutoff) || sample.Timestamp.After(now.Add(30*time.Second)) || !finite(sample.RPSPerWorkUnit) || sample.RPSPerWorkUnit <= 0 {
		return history
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		if sample.Timestamp.Before(last.Timestamp) {
			return history
		}
		if sample.Timestamp.Equal(last.Timestamp) {
			history[len(history)-1] = sample
			return history
		}
	}
	history = append(history, sample)
	if len(history) > MaxSamples {
		history = append([]CapacitySample(nil), history[len(history)-MaxSamples:]...)
	}
	return history
}

// CapacityEstimate extrapolates healthy successful throughput efficiency to the
// physical per-replica constrained-slot limit, then applies a margin to derive
// the conservative safe operating capacity.
type CapacityEstimate struct {
	ConservativeRPSPerWorkUnit  float64
	EstimatedPerReplicaCapacity float64
	SafePerReplicaCapacity      float64
}

// SafePerReplicaCapacity uses the nearest-rank 20th percentile of healthy
// successful RPS per aggregate held-slot occupancy unit. It estimates capacity
// at the physical semaphore ceiling, then applies the safety margin once.
func SafePerReplicaCapacity(samples []CapacitySample, physicalSlotLimit, margin float64) (CapacityEstimate, bool) {
	if len(samples) < MinimumCapacitySamples || !finite(physicalSlotLimit) || physicalSlotLimit <= 0 || !finite(margin) || margin <= 0 || margin > 1 {
		return CapacityEstimate{}, false
	}
	values := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if sample.Timestamp.IsZero() || !finite(sample.RPSPerWorkUnit) || sample.RPSPerWorkUnit <= 0 {
			return CapacityEstimate{}, false
		}
		values = append(values, sample.RPSPerWorkUnit)
	}
	sort.Float64s(values)
	index := int(math.Ceil(0.20*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	estimatedCapacity := values[index] * physicalSlotLimit
	safeCapacity := estimatedCapacity * margin
	if !finite(estimatedCapacity) || estimatedCapacity <= 0 || !finite(safeCapacity) || safeCapacity <= 0 {
		return CapacityEstimate{}, false
	}
	return CapacityEstimate{
		ConservativeRPSPerWorkUnit:  values[index],
		EstimatedPerReplicaCapacity: estimatedCapacity,
		SafePerReplicaCapacity:      safeCapacity,
	}, true
}

func pruneSamples(history []Sample, now time.Time) []Sample {
	cutoff := now.Add(-HistoryWindow)
	kept := history[:0]
	for _, sample := range history {
		if !sample.Timestamp.Before(cutoff) && !sample.Timestamp.After(now.Add(30*time.Second)) && finiteNonnegative(sample.RequestRate) {
			kept = append(kept, sample)
		}
	}
	if len(kept) > MaxSamples {
		kept = kept[len(kept)-MaxSamples:]
	}
	return kept
}

func finite(value float64) bool            { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func finiteNonnegative(value float64) bool { return finite(value) && value >= 0 }
