package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var histogramBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5}

type telemetry struct {
	mu                          sync.Mutex
	requests, errors, durations uint64
	durationSum                 float64
	active                      int64
	activeWork                  int64
	buckets                     []uint64
	dbRequests, dbErrors        uint64
	dbDurations                 uint64
	dbDurationSum               float64
	dbActive                    int64
	dbBuckets                   []uint64
}

func newTelemetry() *telemetry {
	return &telemetry{buckets: make([]uint64, len(histogramBuckets)), dbBuckets: make([]uint64, len(histogramBuckets))}
}

func (t *telemetry) begin()     { t.mu.Lock(); t.active++; t.mu.Unlock() }
func (t *telemetry) beginWork() { t.mu.Lock(); t.activeWork++; t.mu.Unlock() }
func (t *telemetry) endWork()   { t.mu.Lock(); t.activeWork--; t.mu.Unlock() }
func (t *telemetry) beginDB()   { t.mu.Lock(); t.dbActive++; t.mu.Unlock() }

func (t *telemetry) observeDB(duration time.Duration, failed bool) {
	seconds := duration.Seconds()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dbRequests++
	t.dbActive--
	if failed {
		t.dbErrors++
	}
	t.dbDurations++
	t.dbDurationSum += seconds
	for i, boundary := range histogramBuckets {
		if seconds <= boundary {
			t.dbBuckets[i]++
		}
	}
}
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
	fmt.Fprintf(w, "# HELP db_requests_total Total database requests attempted by inventory-service.\n# TYPE db_requests_total counter\ndb_requests_total %d\n", t.dbRequests)
	fmt.Fprintf(w, "# HELP db_errors_total Total database requests that failed.\n# TYPE db_errors_total counter\ndb_errors_total %d\n", t.dbErrors)
	fmt.Fprintf(w, "# HELP db_active_requests Current in-flight database requests.\n# TYPE db_active_requests gauge\ndb_active_requests %d\n", t.dbActive)
	fmt.Fprintln(w, "# HELP db_request_duration_seconds Duration of application-observed database requests in seconds.")
	fmt.Fprintln(w, "# TYPE db_request_duration_seconds histogram")
	for i, boundary := range histogramBuckets {
		fmt.Fprintf(w, "db_request_duration_seconds_bucket{le=\"%s\"} %d\n", strconv.FormatFloat(boundary, 'f', -1, 64), t.dbBuckets[i])
	}
	fmt.Fprintf(w, "db_request_duration_seconds_bucket{le=\"+Inf\"} %d\ndb_request_duration_seconds_sum %f\ndb_request_duration_seconds_count %d\n", t.dbDurations, t.dbDurationSum, t.dbDurations)
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
	db          *sql.DB
	dbDelay     time.Duration
}

const inventoryQuery = `WITH waited AS MATERIALIZED (SELECT pg_sleep($1::double precision))
SELECT item.sku FROM inventory_items AS item CROSS JOIN waited ORDER BY item.id`

func (s *server) inventoryItems(ctx context.Context) (items []string, err error) {
	started := time.Now()
	s.telemetry.beginDB()
	defer func() { s.telemetry.observeDB(time.Since(started), err != nil) }()
	rows, err := s.db.QueryContext(ctx, inventoryQuery, s.dbDelay.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sku string
		if err = rows.Scan(&sku); err != nil {
			return nil, err
		}
		items = append(items, sku)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
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
	dbCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := s.inventoryItems(dbCtx)
	if err != nil {
		failed = true
		http.Error(w, "inventory database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Items []string `json:"items"`
	}{Items: items}); err != nil {
		failed = true
	}
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	work := time.Duration(envInt("INVENTORY_WORK_MS", 25)) * time.Millisecond
	dbDelay := time.Duration(envInt("INVENTORY_DB_DELAY_MS", 0)) * time.Millisecond
	concurrency := envInt("INVENTORY_CONCURRENCY", 20)
	if concurrency < 1 {
		concurrency = 1
	}
	dbUser := os.Getenv("DATABASE_USER")
	dbPassword := os.Getenv("DATABASE_PASSWORD")
	if dbUser == "" || dbPassword == "" {
		logger.Error("DATABASE_USER and DATABASE_PASSWORD must be configured")
		os.Exit(1)
	}
	dbURL := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(dbUser, dbPassword),
		Host:   net.JoinHostPort(envString("DATABASE_HOST", "postgres.optiscale-demo.svc.cluster.local"), envString("DATABASE_PORT", "5432")),
		Path:   envString("DATABASE_NAME", "optiscale"),
	}
	query := dbURL.Query()
	query.Set("sslmode", "disable")
	dbURL.RawQuery = query.Encode()
	db, err := sql.Open("pgx", dbURL.String())
	if err != nil {
		logger.Error("open inventory database", "error", err)
		os.Exit(1)
	}
	maxDBConnections := envInt("DATABASE_MAX_OPEN_CONNS", 40)
	if maxDBConnections < 1 {
		maxDBConnections = 1
	}
	db.SetMaxOpenConns(maxDBConnections)
	db.SetMaxIdleConns(maxDBConnections)
	s := &server{telemetry: newTelemetry(), work: work, concurrency: make(chan struct{}, concurrency), db: db, dbDelay: dbDelay}
	mux := http.NewServeMux()
	mux.HandleFunc("/inventory", s.inventory)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			http.Error(w, "database not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { s.telemetry.write(w) })
	httpServer := &http.Server{Addr: ":8080", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("inventory service listening", "address", httpServer.Addr, "work", work.String(), "dbDelay", dbDelay.String(), "concurrency", concurrency, "dbMaxConnections", maxDBConnections)
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
	if err := db.Close(); err != nil {
		logger.Error("close inventory database", "error", err)
	}
}

func envString(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
