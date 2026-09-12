package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPMetricsSeparateTotalAndFailedRequests(t *testing.T) {
	metrics := newTelemetry()
	metrics.observe(25*time.Millisecond, false)
	metrics.observe(50*time.Millisecond, true)

	response := httptest.NewRecorder()
	metrics.write(response)
	body := response.Body.String()
	for _, want := range []string{
		"http_requests_total 2",
		"http_errors_total 1",
		"http_request_duration_seconds_count 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
}

func TestRequestDurationHistogramUsesCumulativeBucketsAroundSLO(t *testing.T) {
	metrics := newTelemetry()
	metrics.observe(4*time.Millisecond, false)
	metrics.observe(76*time.Millisecond, false)
	metrics.observe(250*time.Millisecond, false)
	metrics.observe(276*time.Millisecond, false)
	metrics.observe(6*time.Second, false)

	response := httptest.NewRecorder()
	metrics.write(response)
	body := response.Body.String()
	wantBuckets := []string{
		`http_request_duration_seconds_bucket{le="0.005"} 1`,
		`http_request_duration_seconds_bucket{le="0.01"} 1`,
		`http_request_duration_seconds_bucket{le="0.025"} 1`,
		`http_request_duration_seconds_bucket{le="0.05"} 1`,
		`http_request_duration_seconds_bucket{le="0.075"} 1`,
		`http_request_duration_seconds_bucket{le="0.1"} 2`,
		`http_request_duration_seconds_bucket{le="0.125"} 2`,
		`http_request_duration_seconds_bucket{le="0.15"} 2`,
		`http_request_duration_seconds_bucket{le="0.175"} 2`,
		`http_request_duration_seconds_bucket{le="0.2"} 2`,
		`http_request_duration_seconds_bucket{le="0.225"} 2`,
		`http_request_duration_seconds_bucket{le="0.25"} 3`,
		`http_request_duration_seconds_bucket{le="0.275"} 3`,
		`http_request_duration_seconds_bucket{le="0.3"} 4`,
		`http_request_duration_seconds_bucket{le="0.35"} 4`,
		`http_request_duration_seconds_bucket{le="0.4"} 4`,
		`http_request_duration_seconds_bucket{le="0.5"} 4`,
		`http_request_duration_seconds_bucket{le="0.75"} 4`,
		`http_request_duration_seconds_bucket{le="1"} 4`,
		`http_request_duration_seconds_bucket{le="2"} 4`,
		`http_request_duration_seconds_bucket{le="5"} 4`,
		`http_request_duration_seconds_bucket{le="+Inf"} 5`,
		`http_request_duration_seconds_sum 6.606000`,
		`http_request_duration_seconds_count 5`,
	}
	for _, want := range wantBuckets {
		if !strings.Contains(body, want) {
			t.Errorf("histogram output missing %q:\n%s", want, body)
		}
	}
}

func TestReadyzWithZeroDelayIsImmediatelyReady(t *testing.T) {
	s := &server{telemetry: newTelemetry(), startedAt: time.Now()}
	response := httptest.NewRecorder()
	s.handler().ServeHTTP(response, httptest.NewRequest("GET", "/readyz", nil))

	if response.Code != http.StatusOK || response.Body.String() != "ready\n" {
		t.Fatalf("/readyz = %d %q; want immediate 200 ready", response.Code, response.Body.String())
	}
}

func TestReadyzWaitsForConfiguredDelayButHealthzDoesNot(t *testing.T) {
	delay := time.Hour
	s := &server{telemetry: newTelemetry(), startedAt: time.Now(), readinessDelay: delay}
	handler := s.handler()

	readyResponse := httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, httptest.NewRequest("GET", "/readyz", nil))
	if readyResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz before delay = %d; want %d", readyResponse.Code, http.StatusServiceUnavailable)
	}

	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, httptest.NewRequest("GET", "/healthz", nil))
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("/healthz during readiness delay = %d; want %d", healthResponse.Code, http.StatusOK)
	}

	s.startedAt = time.Now().Add(-delay)
	readyResponse = httptest.NewRecorder()
	handler.ServeHTTP(readyResponse, httptest.NewRequest("GET", "/readyz", nil))
	if readyResponse.Code != http.StatusOK || readyResponse.Body.String() != "ready\n" {
		t.Fatalf("/readyz after delay = %d %q; want 200 ready", readyResponse.Code, readyResponse.Body.String())
	}
}

func TestLocalWorkCounterTracksElapsedWorkDuration(t *testing.T) {
	s := &server{telemetry: newTelemetry(), work: 5 * time.Millisecond}
	duration := s.performLocalWork()

	response := httptest.NewRecorder()
	s.telemetry.write(response)
	body := response.Body.String()
	if duration <= 0 {
		t.Fatal("positive configured local work produced no measured duration")
	}
	wantCounter := fmt.Sprintf("http_local_work_seconds_total %.9f", duration.Seconds())
	if !strings.Contains(body, wantCounter) {
		t.Errorf("local-work counter missing measured duration %q:\n%s", wantCounter, body)
	}
	if !strings.Contains(body, "http_active_work 0") {
		t.Errorf("active-work gauge did not return to zero after completion:\n%s", body)
	}
}

func TestLocalWorkGaugeRemainsInstantaneousAndCounterIsCumulative(t *testing.T) {
	metrics := newTelemetry()
	metrics.beginWork()
	response := httptest.NewRecorder()
	metrics.write(response)
	if !strings.Contains(response.Body.String(), "http_active_work 1") {
		t.Fatalf("active-work gauge while working:\n%s", response.Body.String())
	}

	metrics.endWork(45 * time.Millisecond)
	response = httptest.NewRecorder()
	metrics.write(response)
	body := response.Body.String()
	if !strings.Contains(body, "http_active_work 0") {
		t.Errorf("active-work gauge after work completion:\n%s", body)
	}
	if !strings.Contains(body, "http_local_work_seconds_total 0.045000000") {
		t.Errorf("cumulative work counter did not add 45ms:\n%s", body)
	}
}

func TestZeroLocalWorkDoesNotIncrementCounter(t *testing.T) {
	s := &server{telemetry: newTelemetry(), work: 0}
	duration := s.performLocalWork()
	if duration != 0 {
		t.Fatalf("zero configured work measured %s; want 0", duration)
	}

	response := httptest.NewRecorder()
	s.telemetry.write(response)
	body := response.Body.String()
	if !strings.Contains(body, "http_local_work_seconds_total 0.000000000") {
		t.Errorf("zero work materially incremented counter:\n%s", body)
	}
	if !strings.Contains(body, "http_active_work 0") {
		t.Errorf("active-work gauge did not return to zero:\n%s", body)
	}
}

func TestConcurrencySlotAndQueueTelemetryMeasureSeparateIntervals(t *testing.T) {
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls atomic.Int32
	inventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			close(firstEntered)
			<-releaseFirst
		case 2:
			close(secondEntered)
			<-releaseSecond
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer inventory.Close()

	s := &server{
		telemetry: newTelemetry(), work: 5 * time.Millisecond,
		concurrency: make(chan struct{}, 1), inventoryURL: inventory.URL,
		httpClient: inventory.Client(),
	}
	var requests sync.WaitGroup
	request := func() {
		defer requests.Done()
		s.root(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}

	requests.Add(1)
	go request()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not reach inventory")
	}

	requests.Add(1)
	go request()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.telemetry.mu.Lock()
		waiters, inUse := s.telemetry.concurrencyQueueWaiters, s.telemetry.concurrencySlotsInUse
		s.telemetry.mu.Unlock()
		if waiters == 1 {
			if inUse != 1 {
				t.Fatalf("while queued: slots in use=%d, want 1", inUse)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not wait for the occupied semaphore slot")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(120 * time.Millisecond)
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second request did not acquire the released slot and reach inventory")
	}
	time.Sleep(20 * time.Millisecond)
	close(releaseSecond)
	requests.Wait()

	response := httptest.NewRecorder()
	s.telemetry.write(response)
	body := response.Body.String()
	slotSeconds := metricValue(t, body, "http_concurrency_slot_seconds_total")
	queueSeconds := metricValue(t, body, "http_concurrency_queue_seconds_total")
	if queueSeconds < 0.08 {
		t.Fatalf("queue wait %.3fs did not include the controlled wait for a slot", queueSeconds)
	}
	if slotSeconds <= queueSeconds+0.02 {
		t.Fatalf("slot time %.3fs did not include local work and downstream wait beyond queue time %.3fs", slotSeconds, queueSeconds)
	}
	if slotSeconds >= queueSeconds+0.10 {
		t.Fatalf("slot time %.3fs appears to include queue wait %.3fs; expected only held-slot intervals", slotSeconds, queueSeconds)
	}
	for _, want := range []string{"http_concurrency_slots_in_use 0", "http_concurrency_queue_waiters 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("concurrency gauge missing %q:\n%s", want, body)
		}
	}
}

func metricValue(t *testing.T, exposition, name string) float64 {
	t.Helper()
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, name+" ") {
			value, err := strconv.ParseFloat(strings.TrimPrefix(line, name+" "), 64)
			if err != nil {
				t.Fatalf("parse %s value: %v", name, err)
			}
			return value
		}
	}
	t.Fatalf("metric %s not found in exposition:\n%s", name, exposition)
	return 0
}
