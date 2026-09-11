package prometheus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientQuery(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		wantErr      error
		wantAnyError bool
	}{
		{"success", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[{"value":["1704067200","251.5"]}]}}`, nil, false},
		{"missing data", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`, ErrNoData, false},
		{"malformed JSON", http.StatusOK, `{`, ErrMalformedResponse, false},
		{"malformed value", http.StatusOK, `{"status":"success","data":{"result":[{"value":["1704067200","wat"]}]}}`, ErrMalformedResponse, false},
		{"NaN", http.StatusOK, `{"status":"success","data":{"result":[{"value":["1704067200","NaN"]}]}}`, ErrNonFiniteValue, false},
		{"HTTP error", http.StatusBadGateway, `upstream failed`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c, err := NewClient(srv.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Query(context.Background(), "up")
			if tt.wantAnyError && err == nil {
				t.Fatal("expected HTTP status error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error=%v, want errors.Is(%v)", err, tt.wantErr)
			}
			if tt.wantErr == nil && tt.name == "success" {
				if err != nil || got.Value != 251.5 || got.Timestamp.IsZero() {
					t.Fatalf("got=%+v err=%v", got, err)
				}
			}
		})
	}
}
