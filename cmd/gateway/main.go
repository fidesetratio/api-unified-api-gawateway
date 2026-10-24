// Command gateway runs the Unified Payment Gateway HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"unifiedgw/internal/api"
	"unifiedgw/internal/auth"
	"unifiedgw/internal/config"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
	"unifiedgw/internal/worker"
	"unifiedgw/migrations"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	dir, err := config.LoadDefaultDotenv()
	if err != nil {
		return err
	}
	getenv, err := config.WithDefaults(os.Getenv, filepath.Join(dir, config.DefaultKeyFile))
	if err != nil {
		return err
	}
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	log.Info("starting", "env", cfg.Env, "addr", cfg.Addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Convenience for local development only; production migrates with gatewayctl.
	autoMigrate := cfg.Env != config.EnvProduction && os.Getenv("GATEWAY_AUTO_MIGRATE") != "0"
	if autoMigrate {
		if err := store.EnsureDatabase(ctx, cfg.DatabaseURL.Reveal()); err != nil {
			return err
		}
	}
	pool, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer pool.Close()

	if autoMigrate {
		applied, err := store.Migrate(ctx, pool, migrations.FS)
		if err != nil {
			return err
		}
		log.Info("migrations applied", "versions", applied)
	}

	registry, err := service.NewRegistry(cfg.FakeProviderAllowed())
	if err != nil {
		return err
	}

	configs := store.NewConfigs(pool)
	clients := store.NewClients(pool)
	payments := store.NewPayments(pool)
	paySvc := service.NewPayments(payments, configs, clients, cfg.Keys, registry, log, service.Options{Owner: hostOwner()})
	adminSvc := service.NewAdmin(configs, clients, cfg.Keys, cfg.FakeProviderAllowed())
	settle := service.NewSettlement(payments, configs, cfg.Keys, registry, log)

	go worker.Run(ctx, log, worker.Jobs{
		ExpireDue:   settle.ExpireDue,
		Reconcile:   paySvc.ResumeCreating,
		Interval:    30 * time.Second,
		Description: "expiry sweeper and reconciler",
	})

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: api.NewRouter(api.Deps{
			Log:                log,
			Auth:               auth.New(clients),
			Payments:           paySvc,
			Admin:              adminSvc,
			Settlement:         settle,
			Ready:              func(c context.Context) error { return pool.Ping(c) },
			AllowInsecureAdmin: cfg.Env == config.EnvDev,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func hostOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "gateway"
	}
	return host
}
