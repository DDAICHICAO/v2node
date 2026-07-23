package accessaudit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeBatchSpool[T any] struct {
	mu      sync.Mutex
	batches [][]T
	block   chan struct{}
	err     error
}

func (s *fakeBatchSpool[T]) EnqueueBatch(events []T) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]T(nil), events...))
	return s.err
}

func TestPersistBatcherUsesBoundedSingleWriterBatches(t *testing.T) {
	spool := &fakeBatchSpool[int]{}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 32, BatchSize: 10,
		BatchWindow: 10 * time.Millisecond, SubmitTimeout: time.Second,
	})
	results := make([]chan error, 25)
	for value := 0; value < 25; value++ {
		results[value] = make(chan error, 1)
		batcher.requests <- persistRequest[int]{event: value, result: results[value]}
	}
	batcher.Start()
	defer func() {
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()
	for _, result := range results {
		if err := <-result; err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	spool.mu.Lock()
	defer spool.mu.Unlock()
	total := 0
	for _, batch := range spool.batches {
		if len(batch) > 10 {
			t.Fatalf("oversized batch: %d", len(batch))
		}
		total += len(batch)
	}
	if total != 25 || len(spool.batches) != 3 {
		t.Fatalf("batches=%d total=%d", len(spool.batches), total)
	}
}

func TestPersistBatcherTimesOutWithoutUnboundedWorkers(t *testing.T) {
	block := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: 30 * time.Millisecond,
	})
	batcher.Start()
	if err := batcher.Submit(1); !errors.Is(err, ErrPersistenceTimeout) {
		t.Fatalf("first submit error=%v", err)
	}
	if err := batcher.Submit(2); !errors.Is(err, ErrPersistenceTimeout) {
		t.Fatalf("second submit error=%v", err)
	}
	status := batcher.Status()
	if status.HighWatermark > 1 || status.Timeouts != 2 {
		t.Fatalf("unexpected status: %#v", status)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := batcher.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}
