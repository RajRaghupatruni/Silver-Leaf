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
	mu          sync.Mutex
	requests    uint64
	errors      uint64
	durations   uint64
	durationSum float64
	buckets     []uint64
}

func newTelemetry() *telemetry { return &telemetry{buckets: make([]uint64, len(histogramBuckets))} }

func (t *telemetry) observe(duration time.Duration, failed bool) {
	seconds := duration.Seconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests++
	if failed {
		t.errors++
	}
	t.durations++
	t.durationSum += seconds
	for i, boundary := range histogramBuckets {
		if seconds <= boundary {
			t.buckets[i]++
		}
	}
}

func (t *telemetry) write(w http.ResponseWriter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP http_requests_total Total HTTP requests handled by the demo application.\n# TYPE http_requests_total counter\nhttp_requests_total %d\n", t.requests)
	fmt.Fprintf(w, "# HELP http_errors_total Total HTTP requests that returned an error.\n# TYPE http_errors_total counter\nhttp_errors_total %d\n", t.errors)
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

func (s *server) root(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	failed := r.URL.Query().Get("fail") == "1"
	s.concurrency <- struct{}{}
	defer func() { <-s.concurrency; s.telemetry.observe(time.Since(started), failed) }()
	time.Sleep(s.work)
	if failed {
		http.Error(w, "intentional demo error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OptiScale demo app\n"))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	work := time.Duration(envDuration("DEMO_WORK_MS", 25)) * time.Millisecond
	concurrency := envInt("DEMO_CONCURRENCY", 4)
	if concurrency < 1 {
		concurrency = 1
	}
	s := &server{telemetry: newTelemetry(), work: work, concurrency: make(chan struct{}, concurrency)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.root)
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
		logger.Info("demo app listening", "address", httpServer.Addr, "work", work.String(), "concurrency", concurrency)
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
func envDuration(name string, fallback int) int { return envInt(name, fallback) }
