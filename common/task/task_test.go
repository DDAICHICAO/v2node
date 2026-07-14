package task

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestTaskStartContinuesAfterExecuteError(t *testing.T) {
	var attempts atomic.Int32
	periodic := &Task{
		Name:     "retry-after-error",
		Interval: 10 * time.Millisecond,
		Execute: func(context.Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("temporary failure")
			}
			return nil
		},
	}

	if err := periodic.Start(false); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer periodic.Close()

	deadline := time.After(300 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		if got := attempts.Load(); got >= 3 {
			return
		}

		select {
		case <-deadline:
			t.Fatalf("expected task to continue after execute errors, got %d attempts", attempts.Load())
		case <-ticker.C:
		}
	}
}

func TestTaskTimeoutDoesNotReloadUnlessEnabled(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	periodic := &Task{
		Name:     "panel-sync",
		Interval: 5 * time.Millisecond,
		Execute: func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		},
		ReloadCh: reloadCh,
	}
	if err := periodic.ExecuteWithTimeout(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v, want deadline exceeded", err)
	}
	select {
	case <-reloadCh:
		t.Fatal("panel task timeout triggered reload")
	default:
	}
}

func TestTaskTimeoutReloadsWhenExplicitlyEnabled(t *testing.T) {
	reloadCh := make(chan struct{}, 1)
	periodic := &Task{
		Name:            "certificate-recovery",
		Interval:        5 * time.Millisecond,
		ReloadOnTimeout: true,
		Execute: func(context.Context) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		},
		ReloadCh: reloadCh,
	}
	if err := periodic.ExecuteWithTimeout(); err != nil {
		t.Fatalf("ExecuteWithTimeout error=%v", err)
	}
	select {
	case <-reloadCh:
	default:
		t.Fatal("explicit reload policy did not signal reload")
	}
}
