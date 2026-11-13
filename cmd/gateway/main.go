// Command gateway runs the simulated payment gateway.
//
//	gateway serve            run the HTTP API and the background workers (default)
//	gateway migrate          apply database migrations and exit
//	gateway create-merchant  create a merchant and print its API key once
//	gateway healthcheck      call /healthz (for container health checks)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kmadsdev/simple-payment-gateway/internal/acquirer"
	"github.com/kmadsdev/simple-payment-gateway/internal/httpapi"
	"github.com/kmadsdev/simple-payment-gateway/internal/observability"
	"github.com/kmadsdev/simple-payment-gateway/internal/payment"
	"github.com/kmadsdev/simple-payment-gateway/internal/resilience"
	"github.com/kmadsdev/simple-payment-gateway/internal/service"
	"github.com/kmadsdev/simple-payment-gateway/internal/store"
	"github.com/kmadsdev/simple-payment-gateway/internal/webhook"
	"github.com/kmadsdev/simple-payment-gateway/migrations"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	log := observability.NewLogger(os.Stdout, observability.ParseLevel(env("LOG_LEVEL", "info")))
	var err error
	switch cmd {
	case "serve":
		err = serve(log)
	case "migrate":
		err = migrate(log)
	case "create-merchant":
		err = createMerchant(log, args)
	case "healthcheck":
		err = healthcheck()
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		log.Error("exit", "command", cmd, "error", err.Error())
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(key, "")); err == nil {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func connect(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := env("DATABASE_URL", "")
	if dsn == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = int32(envInt("DB_MAX_CONNS", 10))
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database ping: %w", err)
	}
	return pool, nil
}

func migrate(log *slog.Logger) error {
	ctx := context.Background()
	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrations.Up(ctx, pool); err != nil {
		return err
	}
	log.Info("migrations applied")
	return nil
}

func newService(pool *pgxpool.Pool, log *slog.Logger, m *observability.Metrics) *service.Service {
	st := store.New(pool)
	if m != nil {
		st.OnTransition = func(t payment.Transition) { m.Transitions.WithLabelValues(t.Op, string(t.To)).Inc() }
	}
	sim := acquirer.NewSimulator(envDuration("SIM_DELAYED_APPROVAL", 5*time.Second))
	breaker := &resilience.Breaker{
		Threshold: envInt("ACQUIRER_BREAKER_THRESHOLD", 5),
		Cooldown:  envDuration("ACQUIRER_BREAKER_COOLDOWN", 30*time.Second),
	}
	acq := &acquirer.Resilient{
		Inner:          sim,
		AttemptTimeout: envDuration("ACQUIRER_ATTEMPT_TIMEOUT", 2*time.Second),
		TotalBudget:    envDuration("ACQUIRER_TOTAL_BUDGET", 5*time.Second),
		MaxAttempts:    envInt("ACQUIRER_MAX_ATTEMPTS", 3),
		Backoff:        resilience.Backoff{Base: 100 * time.Millisecond, Max: time.Second},
		Breaker:        breaker,
	}
	if m != nil {
		acq.Observe = m.ObserveAcquirer
		breaker.OnChange = func(s resilience.State) { m.SetBreakerState(int(s)) }
	}
	svc := &service.Service{
		Store: st, Acq: acq, Cfg: service.Config{
			AuthorizationTTL: envDuration("AUTHORIZATION_TTL", 7*24*time.Hour),
			PendingTTL:       envDuration("PENDING_TTL", 30*time.Minute),
			PixMerchantName:  env("PIX_MERCHANT_NAME", "Simulated Store"),
			PixCity:          env("PIX_CITY", "Sao Paulo"),
		},
		Log: log,
	}
	if env("SIMULATOR_ENABLED", "true") == "true" {
		svc.Sim = sim
	}
	return svc
}

func serve(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if env("MIGRATE_ON_START", "true") == "true" {
		if err := migrations.Up(ctx, pool); err != nil {
			return err
		}
	}

	metrics := observability.NewMetrics()
	svc := newService(pool, log, metrics)
	var draining atomic.Bool
	api := &httpapi.API{
		Svc: svc, Store: svc.Store, Metrics: metrics, Log: log, Simulator: svc.Sim != nil, Draining: &draining,
		Limiter: httpapi.NewRateLimiter(envInt("RATE_LIMIT_RPS", 20), envInt("RATE_LIMIT_BURST", 40), nil),
	}
	srv := &http.Server{
		Addr:              env("HTTP_ADDR", ":8080"),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Background workers stop when workCtx ends.
	workCtx, stopWork := context.WithCancel(context.Background())
	defer stopWork()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		svc.RunMaintenance(workCtx, envDuration("MAINTENANCE_INTERVAL", 2*time.Second))
	}()
	dispatcher := &webhook.Dispatcher{
		Outbox: svc.Store, Client: webhook.NewClient(10 * time.Second),
		Backoff:     resilience.Backoff{Base: envDuration("WEBHOOK_BACKOFF_BASE", 2*time.Second), Max: time.Hour},
		MaxAttempts: envInt("WEBHOOK_MAX_ATTEMPTS", 8), Batch: 20, Lease: time.Minute, Log: log,
		OnResult: func(o string) { metrics.WebhookDeliveries.WithLabelValues(o).Inc() },
	}
	go func() {
		defer wg.Done()
		dispatcher.Run(workCtx, time.Second)
	}()

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr, "simulator", svc.Sim != nil)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown: fail readiness, let load balancers notice, stop
	// accepting, finish in-flight requests, then stop the workers.
	log.Info("shutting down")
	draining.Store(true)
	time.Sleep(envDuration("SHUTDOWN_DRAIN_DELAY", 0))
	shutCtx, cancel := context.WithTimeout(context.Background(), envDuration("SHUTDOWN_TIMEOUT", 15*time.Second))
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("http shutdown", "error", err.Error())
	}
	stopWork()
	wg.Wait()
	log.Info("stopped")
	return nil
}

func createMerchant(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("create-merchant", flag.ContinueOnError)
	name := fs.String("name", "", "merchant name (required)")
	webhookURL := fs.String("webhook-url", "", "URL that receives webhooks (optional)")
	secret := fs.String("webhook-secret", env("WEBHOOK_SECRET", ""), "webhook signing secret (default: random)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrations.Up(ctx, pool); err != nil {
		return err
	}
	svc := newService(pool, log, nil)
	nm, err := svc.CreateMerchant(ctx, *name, *webhookURL, *secret)
	if err != nil {
		return err
	}
	// Printed once to the operator's terminal on purpose. Not logged.
	fmt.Printf("merchant_id=%s\napi_key=%s\nwebhook_secret=%s\n", nm.Merchant.ID, nm.APIKey, nm.WebhookSecret)
	return nil
}

func healthcheck() error {
	addr := env("HTTP_ADDR", ":8080")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
}
