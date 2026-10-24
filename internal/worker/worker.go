// Package worker runs the periodic background jobs of the gateway.
package worker

import (
	"context"
	"log/slog"
	"time"
)

// Jobs are the periodic tasks. Each returns how many rows it handled.
type Jobs struct {
	ExpireDue   func(context.Context) (int, error)
	Reconcile   func(context.Context) (int, error)
	Interval    time.Duration
	Description string
}

// Run executes the jobs every Interval until ctx is cancelled. A failing or
// panicking job is logged and retried on the next tick; it never stops the loop.
func Run(ctx context.Context, log *slog.Logger, j Jobs) {
	if j.Interval <= 0 {
		j.Interval = 30 * time.Second
	}
	log.Info("worker started", "jobs", j.Description, "interval", j.Interval.String())
	ticker := time.NewTicker(j.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("worker stopped")
			return
		case <-ticker.C:
			RunOnce(ctx, log, j)
		}
	}
}

// RunOnce runs every job once.
func RunOnce(ctx context.Context, log *slog.Logger, j Jobs) {
	runJob(ctx, log, "expire", j.ExpireDue)
	runJob(ctx, log, "reconcile", j.Reconcile)
}

func runJob(ctx context.Context, log *slog.Logger, name string, f func(context.Context) (int, error)) {
	if f == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("worker job panicked", "job", name, "panic", r)
		}
	}()
	n, err := f(ctx)
	if err != nil {
		log.Error("worker job failed", "job", name, "error", err)
		return
	}
	if n > 0 {
		log.Info("worker job done", "job", name, "handled", n)
	}
}
