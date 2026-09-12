package main

import (
	"net/http/httptest"
	"strings"
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
