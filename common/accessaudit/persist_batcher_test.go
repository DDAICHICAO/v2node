package accessaudit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeBatchSpool[T any] struct {
	mu        sync.Mutex
	batches   [][]T
	block     chan struct{}
	started   chan struct{}
	startOnce sync.Once
	err       error
}

func (s *fakeBatchSpool[T]) EnqueueBatch(events []T) error {
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]T(nil), events...))
	return s.err
}

func TestPersistBatcherTrySubmitReturnsBeforeTransactionCompletes(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	defer func() {
		close(block)
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	if err := batcher.TrySubmit(1); err != nil {
		t.Fatalf("try submit: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not receive admitted event")
	}
}

func TestPersistBatcherTrySubmitRejectsFullQueueImmediately(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{})
	spool := &fakeBatchSpool[int]{block: block, started: started}
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 1, BatchSize: 1,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Hour,
	})
	defer func() {
		close(block)
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()

	if err := batcher.TrySubmit(1); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	if err := batcher.TrySubmit(2); err != nil {
		t.Fatalf("second admission: %v", err)
	}
	if err := batcher.TrySubmit(3); !errors.Is(err, ErrPersistenceQueueFull) {
		t.Fatalf("third admission error=%v", err)
	}
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
	if err := batcher.Submit(1); !errors.Is(err, ErrPersistencePending) {
		t.Fatalf("first submit error=%v", err)
	}
	if err := batcher.Submit(2); !errors.Is(err, ErrPersistencePending) {
		t.Fatalf("second submit error=%v", err)
	}
	if err := batcher.Submit(3); !errors.Is(err, ErrPersistenceTimeout) {
		t.Fatalf("third submit error=%v", err)
	}
	status := batcher.Status()
	if status.HighWatermark > 1 || status.Timeouts != 3 {
		t.Fatalf("unexpected status: %#v", status)
	}
	close(block)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := batcher.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestPersistBatcherReportsTransactionFailureOnceForWholeBatch(t *testing.T) {
	diskErr := errors.New("disk read-only")
	spool := &fakeBatchSpool[int]{err: diskErr}
	var mu sync.Mutex
	callbackCalls := 0
	var failedEvents []int
	batcher := newPersistBatcher(persistBatcherConfig[int]{
		Spool: spool, QueueSize: 10, BatchSize: 10,
		BatchWindow: time.Millisecond, SubmitTimeout: time.Second,
		OnFailure: func(events []int, err error) {
			if !errors.Is(err, diskErr) {
				t.Errorf("callback error=%v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			callbackCalls++
			failedEvents = append(failedEvents, events...)
		},
	})
	results := make([]chan error, 3)
	for value := 1; value <= 3; value++ {
		results[value-1] = make(chan error, 1)
		batcher.requests <- persistRequest[int]{event: value, result: results[value-1]}
	}
	batcher.Start()
	defer func() {
		if err := batcher.Close(context.Background()); err != nil {
			t.Fatalf("close: %v", err)
		}
	}()
	for _, result := range results {
		if err := <-result; !errors.Is(err, diskErr) {
			t.Fatalf("submit error=%v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if callbackCalls != 1 || len(failedEvents) != 3 {
		t.Fatalf("callback calls=%d failed events=%v", callbackCalls, failedEvents)
	}
	if status := batcher.Status(); status.Failures != 3 {
		t.Fatalf("unexpected status: %#v", status)
	}
}
