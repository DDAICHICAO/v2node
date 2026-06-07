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
