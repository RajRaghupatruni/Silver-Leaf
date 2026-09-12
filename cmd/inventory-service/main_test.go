package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDatabaseTelemetryExposesObservedQuerySignals(t *testing.T) {
	metrics := newTelemetry()
	metrics.beginDB()
	metrics.observeDB(850*time.Millisecond, true)

	response := httptest.NewRecorder()
	metrics.write(response)
	body := response.Body.String()
	for _, want := range []string{
		"db_requests_total 1",
		"db_errors_total 1",
		"db_active_requests 0",
		"db_request_duration_seconds_bucket{le=\"1\"} 1",
		"db_request_duration_seconds_bucket{le=\"+Inf\"} 1",
		"db_request_duration_seconds_count 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q:\n%s", want, body)
		}
	}
}
