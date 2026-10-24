// Command gatewayctl is the operator CLI: migrations, API clients, key
// re-encryption, review items and reconciliation.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"unifiedgw/internal/cli"
	"unifiedgw/internal/config"
	"unifiedgw/internal/secrets"
	"unifiedgw/internal/service"
	"unifiedgw/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx))
}

func run(ctx context.Context) int {
	if _, err := config.LoadDefaultDotenv(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	dbURL := os.Getenv("GATEWAY_DATABASE_URL")
	if dbURL == "" {
		var err error
		if dbURL, err = config.DatabaseURL(os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "error: set DB_* in .env or GATEWAY_DATABASE_URL:", err)
			return 1
		}
	}
	if len(os.Args) > 2 && os.Args[1] == "migrate" && os.Args[2] == "up" {
		if err := store.EnsureDatabase(ctx, dbURL); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	pool, err := store.Open(ctx, dbURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer pool.Close()

	// Master keys are optional: only key reencrypt and reconcile need them.
	var keys *secrets.KeyRing
	if spec := os.Getenv("GATEWAY_MASTER_KEYS"); spec != "" {
		keys, err = secrets.ParseKeyRing(spec, os.Getenv("GATEWAY_ACTIVE_KEY_ID"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: master keys:", err)
			return 1
		}
	}

	deps := cli.Deps{Out: os.Stdout, Err: os.Stderr, Pool: pool, Keys: keys, Getenv: os.Getenv}
	if keys != nil {
		deps.Reconcile = reconciler(pool, keys, os.Getenv("GATEWAY_ENV") != "production")
	}
	return cli.Run(ctx, os.Args[1:], deps)
}

// reconciler runs the expiry sweeper and the creating-payment reconciler once.
func reconciler(pool *pgxpool.Pool, keys *secrets.KeyRing, allowFake bool) func(context.Context) (int, int, error) {
	return func(ctx context.Context) (int, int, error) {
		reg, err := service.NewRegistry(allowFake)
		if err != nil {
			return 0, 0, err
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		configs, clients, payments := store.NewConfigs(pool), store.NewClients(pool), store.NewPayments(pool)
		pay := service.NewPayments(payments, configs, clients, keys, reg, log, service.Options{Owner: "gatewayctl"})
		settle := service.NewSettlement(payments, configs, keys, reg, log)

		expired, err := settle.ExpireDue(ctx)
		if err != nil {
			return 0, 0, err
		}
		resumed, err := pay.ResumeCreating(ctx)
		return expired, resumed, err
	}
}
