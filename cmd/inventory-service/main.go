package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

var histogramBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}

type telemetry struct {
	mu                          sync.Mutex
	requests, errors, durations uint64
	durationSum                 float64
	active                      int64
	activeWork                  int64
	buckets                     []uint64
}

func newTelemetry() *telemetry { return &telemetry{buckets: make([]uint64, len(histogramBuckets))} }

func (t *telemetry) begin()     { t.mu.Lock(); t.active++; t.mu.Unlock() }
func (t *telemetry) beginWork() { t.mu.Lock(); t.activeWork++; t.mu.Unlock() }
func (t *telemetry) endWork()   { t.mu.Lock(); t.activeWork--; t.mu.Unlock() }
func (t *telemetry) observe(duration time.Duration, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests++
	if failed {
		t.errors++
	}
	seconds := duration.Seconds()
	t.durations++
	t.durationSum += seconds
	for i, boundary := range histogramBuckets {
		if seconds <= boundary {
			t.buckets[i]++
		}
	}
	t.active--
}

func (t *telemetry) write(w http.ResponseWriter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP http_requests_total Total HTTP requests handled by inventory-service.\n# TYPE http_requests_total counter\nhttp_requests_total %d\n", t.requests)
	fmt.Fprintf(w, "# HELP http_errors_total Total HTTP requests that returned an error.\n# TYPE http_errors_total counter\nhttp_errors_total %d\n", t.errors)
	fmt.Fprintf(w, "# HELP http_active_requests Current in-flight HTTP requests.\n# TYPE http_active_requests gauge\nhttp_active_requests %d\n", t.active)
	fmt.Fprintf(w, "# HELP http_active_work Current requests performing local service work.\n# TYPE http_active_work gauge\nhttp_active_work %d\n", t.activeWork)
	fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request duration in seconds.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	for i, boundary := range histogramBuckets {
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"%s\"} %d\n", strconv.FormatFloat(boundary, 'f', -1, 64), t.buckets[i])
	}
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"+Inf\"} %d\nhttp_request_duration_seconds_sum %f\nhttp_request_duration_seconds_count %d\n", t.durations, t.durationSum, t.durations)
}

type server struct {
	telemetry   *telemetry
	work        time.Duration
	concurrency chan struct{}
}

func (s *server) inventory(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	failed := false
	s.telemetry.begin()
	s.concurrency <- struct{}{}
	defer func() { <-s.concurrency; s.telemetry.observe(time.Since(started), failed) }()
	s.telemetry.beginWork()
	timer := time.NewTimer(s.work)
	defer timer.Stop()
	select {
	case <-r.Context().Done():
		s.telemetry.endWork()
		failed = true
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return
	case <-timer.C:
		s.telemetry.endWork()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{\"items\":[\"widget\",\"gadget\"]}\n"))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	work := time.Duration(envInt("INVENTORY_WORK_MS", 25)) * time.Millisecond
	concurrency := envInt("INVENTORY_CONCURRENCY", 20)
	if concurrency < 1 {
		concurrency = 1
	}
	s := &server{telemetry: newTelemetry(), work: work, concurrency: make(chan struct{}, concurrency)}
	mux := http.NewServeMux()
	mux.HandleFunc("/inventory", s.inventory)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { s.telemetry.write(w) })
	httpServer := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("inventory service listening", "address", httpServer.Addr, "work", work.String(), "concurrency", concurrency)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server stopped", "error", err)
			os.Exit(1)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
