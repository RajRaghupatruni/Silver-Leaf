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

var histogramBuckets = []float64{
	0.005, 0.01, 0.025, 0.05,
	0.075, 0.1, 0.125, 0.15, 0.175,
	0.2, 0.225, 0.25, 0.275, 0.3,
	0.35, 0.4, 0.5, 0.75, 1, 2, 5,
}

type telemetry struct {
	mu                      sync.Mutex
	requests                uint64
	errors                  uint64
	durations               uint64
	durationSum             float64
	buckets                 []uint64
	active                  int64
	activeWork              int64
	localWorkSeconds        float64
	concurrencySlotSeconds  float64
	concurrencyQueueSeconds float64
	concurrencySlotsInUse   int64
	concurrencyQueueWaiters int64
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

func (t *telemetry) begin()     { t.mu.Lock(); t.active++; t.mu.Unlock() }
func (t *telemetry) end()       { t.mu.Lock(); t.active--; t.mu.Unlock() }
func (t *telemetry) beginWork() { t.mu.Lock(); t.activeWork++; t.mu.Unlock() }
func (t *telemetry) endWork(duration time.Duration) {
	t.mu.Lock()
	t.activeWork--
	if duration > 0 {
		t.localWorkSeconds += duration.Seconds()
	}
	t.mu.Unlock()
}

func (t *telemetry) beginSlotQueue() {
	t.mu.Lock()
	t.concurrencyQueueWaiters++
	t.mu.Unlock()
}

func (t *telemetry) acquiredSlot(queueWait time.Duration) {
	t.mu.Lock()
	t.concurrencyQueueWaiters--
	t.concurrencySlotsInUse++
	if queueWait > 0 {
		t.concurrencyQueueSeconds += queueWait.Seconds()
	}
	t.mu.Unlock()
}

func (t *telemetry) releasedSlot(held time.Duration) {
	t.mu.Lock()
	t.concurrencySlotsInUse--
	if held > 0 {
		t.concurrencySlotSeconds += held.Seconds()
	}
	t.mu.Unlock()
}

func (t *telemetry) write(w http.ResponseWriter) {
	t.mu.Lock()
	defer t.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP http_requests_total Total HTTP requests handled by the demo application.\n# TYPE http_requests_total counter\nhttp_requests_total %d\n", t.requests)
	fmt.Fprintf(w, "# HELP http_errors_total Total HTTP requests that returned an error.\n# TYPE http_errors_total counter\nhttp_errors_total %d\n", t.errors)
	fmt.Fprintf(w, "# HELP http_active_requests Current in-flight HTTP requests.\n# TYPE http_active_requests gauge\nhttp_active_requests %d\n", t.active)
	fmt.Fprintf(w, "# HELP http_active_work Current requests performing local demo work.\n# TYPE http_active_work gauge\nhttp_active_work %d\n", t.activeWork)
	fmt.Fprintf(w, "# HELP http_local_work_seconds_total Total seconds spent in local demo request work.\n# TYPE http_local_work_seconds_total counter\nhttp_local_work_seconds_total %.9f\n", t.localWorkSeconds)
	fmt.Fprintf(w, "# HELP http_concurrency_slot_seconds_total Cumulative wall-clock seconds requests held a concurrency slot, excluding time queued for a slot.\n# TYPE http_concurrency_slot_seconds_total counter\nhttp_concurrency_slot_seconds_total %.9f\n", t.concurrencySlotSeconds)
	fmt.Fprintf(w, "# HELP http_concurrency_queue_seconds_total Cumulative wall-clock seconds requests waited to acquire a concurrency slot.\n# TYPE http_concurrency_queue_seconds_total counter\nhttp_concurrency_queue_seconds_total %.9f\n", t.concurrencyQueueSeconds)
	fmt.Fprintf(w, "# HELP http_concurrency_slots_in_use Current concurrency semaphore slots held.\n# TYPE http_concurrency_slots_in_use gauge\nhttp_concurrency_slots_in_use %d\n", t.concurrencySlotsInUse)
	fmt.Fprintf(w, "# HELP http_concurrency_queue_waiters Current requests waiting to acquire a concurrency slot.\n# TYPE http_concurrency_queue_waiters gauge\nhttp_concurrency_queue_waiters %d\n", t.concurrencyQueueWaiters)
	fmt.Fprintln(w, "# HELP http_request_duration_seconds HTTP request duration in seconds.")
	fmt.Fprintln(w, "# TYPE http_request_duration_seconds histogram")
	for i, boundary := range histogramBuckets {
		fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"%s\"} %d\n", strconv.FormatFloat(boundary, 'f', -1, 64), t.buckets[i])
	}
	fmt.Fprintf(w, "http_request_duration_seconds_bucket{le=\"+Inf\"} %d\nhttp_request_duration_seconds_sum %f\nhttp_request_duration_seconds_count %d\n", t.durations, t.durationSum, t.durations)
}

type server struct {
	telemetry      *telemetry
	work           time.Duration
	concurrency    chan struct{}
	inventoryURL   string
	httpClient     *http.Client
	startedAt      time.Time
	readinessDelay time.Duration
}

func (s *server) root(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	failed := r.URL.Query().Get("fail") == "1"
	s.telemetry.begin()
	defer s.telemetry.end()
	s.telemetry.beginSlotQueue()
	queueStarted := time.Now()
	s.concurrency <- struct{}{}
	queueWait := time.Since(queueStarted)
	slotStarted := time.Now()
	s.telemetry.acquiredSlot(queueWait)
	defer func() {
		held := time.Since(slotStarted)
		<-s.concurrency
		s.telemetry.releasedSlot(held)
		s.telemetry.observe(time.Since(started), failed)
	}()
	s.performLocalWork()
	if failed {
		http.Error(w, "intentional demo error", http.StatusInternalServerError)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, s.inventoryURL, nil)
	if err != nil {
		failed = true
		http.Error(w, "inventory request setup failed", http.StatusBadGateway)
		return
	}
	response, err := s.httpClient.Do(request)
	if err != nil || response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		failed = true
		if response != nil {
			_ = response.Body.Close()
		}
		http.Error(w, "inventory unavailable", http.StatusBadGateway)
		return
	}
	_ = response.Body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("OptiScale demo app\n"))
}

func (s *server) performLocalWork() time.Duration {
	s.telemetry.beginWork()
	started := time.Now()
	time.Sleep(s.work)
	duration := time.Since(started)
	if s.work <= 0 {
		duration = 0
	}
	s.telemetry.endWork(duration)
	return duration
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.root)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if time.Since(s.startedAt) < s.readinessDelay {
			http.Error(w, "initialization in progress", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { s.telemetry.write(w) })
	return mux
}

func main() {
	processStartedAt := time.Now()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	work := time.Duration(envDuration("DEMO_WORK_MS", 25)) * time.Millisecond
	readinessDelay := time.Duration(envDuration("DEMO_READY_DELAY_MS", 0)) * time.Millisecond
	if readinessDelay < 0 {
		readinessDelay = 0
	}
	concurrency := envInt("DEMO_CONCURRENCY", 4)
	if concurrency < 1 {
		concurrency = 1
	}
	inventoryURL := os.Getenv("INVENTORY_SERVICE_URL")
	if inventoryURL == "" {
		inventoryURL = "http://inventory-service.optiscale-demo.svc.cluster.local:8080/inventory"
	}
	s := &server{
		telemetry: newTelemetry(), work: work, concurrency: make(chan struct{}, concurrency),
		inventoryURL: inventoryURL, httpClient: &http.Client{Timeout: 2 * time.Second},
		startedAt: processStartedAt, readinessDelay: readinessDelay,
	}

	httpServer := &http.Server{Addr: ":8080", Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("demo app listening", "address", httpServer.Addr, "work", work.String(), "concurrency", concurrency, "readinessDelay", readinessDelay.String())
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
