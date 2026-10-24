package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunOnceSurvivesFailingAndPanickingJobs(t *testing.T) {
	var reconciled atomic.Int32
	RunOnce(context.Background(), quiet(), Jobs{
		ExpireDue: func(context.Context) (int, error) { panic("boom") },
		Reconcile: func(context.Context) (int, error) { reconciled.Add(1); return 0, nil },
	})
	if reconciled.Load() != 1 {
		t.Error("a panicking job stopped the next job")
	}
	RunOnce(context.Background(), quiet(), Jobs{
		ExpireDue: func(context.Context) (int, error) { return 0, errors.New("db down") },
		Reconcile: func(context.Context) (int, error) { reconciled.Add(1); return 3, nil },
	})
	if reconciled.Load() != 2 {
		t.Error("a failing job stopped the next job")
	}
	RunOnce(context.Background(), quiet(), Jobs{}) // nil jobs are ignored
}

func TestRunTicksAndStopsOnCancel(t *testing.T) {
	var ticks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, quiet(), Jobs{
			ExpireDue: func(context.Context) (int, error) { ticks.Add(1); return 0, nil },
			Interval:  20 * time.Millisecond,
		})
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancel")
	}
	if ticks.Load() < 2 {
		t.Errorf("ticks = %d, want several", ticks.Load())
	}
}
