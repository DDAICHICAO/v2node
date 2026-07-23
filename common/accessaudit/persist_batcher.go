package accessaudit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrPersistenceTimeout = errors.New("access audit persistence timeout")
	ErrPersistenceClosed  = errors.New("access audit persistence is closed")
)

type batchSpool[T any] interface {
	EnqueueBatch([]T) error
}

type persistRequest[T any] struct {
	event  T
	result chan error
}

type persistBatcherConfig[T any] struct {
	Spool         batchSpool[T]
	QueueSize     int
	BatchSize     int
	BatchWindow   time.Duration
	SubmitTimeout time.Duration
}

type PersistBatcherStatus struct {
	QueueDepth    uint64
	HighWatermark uint64
	Timeouts      uint64
	Failures      uint64
}

type persistBatcher[T any] struct {
	config    persistBatcherConfig[T]
	requests  chan persistRequest[T]
	closeCh   chan struct{}
	doneCh    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	acceptMu  sync.RWMutex
	closed    atomic.Bool
	highWater atomic.Uint64
	timeouts  atomic.Uint64
	failures  atomic.Uint64
}

func newPersistBatcher[T any](config persistBatcherConfig[T]) *persistBatcher[T] {
	if config.QueueSize <= 0 {
		config.QueueSize = 1
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 1
	}
	if config.BatchSize > 1000 {
		config.BatchSize = 1000
	}
	if config.BatchWindow <= 0 {
		config.BatchWindow = 10 * time.Millisecond
	}
	if config.SubmitTimeout <= 0 {
		config.SubmitTimeout = time.Second
	}
	return &persistBatcher[T]{
		config:   config,
		requests: make(chan persistRequest[T], config.QueueSize),
		closeCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (b *persistBatcher[T]) Start() {
	if b == nil {
		return
	}
	b.startOnce.Do(func() {
		go b.loop()
	})
}

func (b *persistBatcher[T]) Submit(event T) error {
	if b == nil || b.config.Spool == nil {
		return ErrPersistenceClosed
	}
	b.Start()
	deadline := time.Now().Add(b.config.SubmitTimeout)
	request := persistRequest[T]{
		event:  event,
		result: make(chan error, 1),
	}

	b.acceptMu.RLock()
	if b.closed.Load() {
		b.acceptMu.RUnlock()
		return ErrPersistenceClosed
	}
	timer := time.NewTimer(time.Until(deadline))
	select {
	case b.requests <- request:
		if !timer.Stop() {
			<-timer.C
		}
		b.recordDepth()
		b.acceptMu.RUnlock()
	case <-timer.C:
		b.acceptMu.RUnlock()
		b.timeouts.Add(1)
		return ErrPersistenceTimeout
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		b.timeouts.Add(1)
		return ErrPersistenceTimeout
	}
	timer.Reset(remaining)
	defer timer.Stop()
	select {
	case err := <-request.result:
		return err
	case <-timer.C:
		b.timeouts.Add(1)
		return ErrPersistenceTimeout
	}
}

func (b *persistBatcher[T]) Status() PersistBatcherStatus {
	if b == nil {
		return PersistBatcherStatus{}
	}
	return PersistBatcherStatus{
		QueueDepth:    uint64(len(b.requests)),
		HighWatermark: b.highWater.Load(),
		Timeouts:      b.timeouts.Load(),
		Failures:      b.failures.Load(),
	}
}

func (b *persistBatcher[T]) Close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.Start()
	b.closeOnce.Do(func() {
		b.acceptMu.Lock()
		b.closed.Store(true)
		close(b.closeCh)
		b.acceptMu.Unlock()
	})
	select {
	case <-b.doneCh:
		return nil
	case <-ctx.Done():
		return ErrPersistenceTimeout
	}
}

func (b *persistBatcher[T]) loop() {
	defer close(b.doneCh)
	for {
		select {
		case first := <-b.requests:
			b.persistBatch(b.collect(first, true))
		case <-b.closeCh:
			b.drain()
			return
		}
	}
}

func (b *persistBatcher[T]) collect(first persistRequest[T], wait bool) []persistRequest[T] {
	batch := make([]persistRequest[T], 0, b.config.BatchSize)
	batch = append(batch, first)
	if len(batch) >= b.config.BatchSize {
		return batch
	}
	var timer *time.Timer
	var timerCh <-chan time.Time
	if wait {
		timer = time.NewTimer(b.config.BatchWindow)
		timerCh = timer.C
		defer timer.Stop()
	}
	for len(batch) < b.config.BatchSize {
		select {
		case request := <-b.requests:
			batch = append(batch, request)
		case <-timerCh:
			return batch
		default:
			if wait {
				select {
				case request := <-b.requests:
					batch = append(batch, request)
				case <-timerCh:
					return batch
				}
				continue
			}
			return batch
		}
	}
	return batch
}

func (b *persistBatcher[T]) persistBatch(batch []persistRequest[T]) {
	events := make([]T, 0, len(batch))
	for _, request := range batch {
		events = append(events, request.event)
	}
	err := b.config.Spool.EnqueueBatch(events)
	if err != nil {
		b.failures.Add(uint64(len(batch)))
	}
	for _, request := range batch {
		request.result <- err
	}
}

func (b *persistBatcher[T]) drain() {
	for {
		select {
		case first := <-b.requests:
			b.persistBatch(b.collect(first, false))
		default:
			return
		}
	}
}

func (b *persistBatcher[T]) recordDepth() {
	depth := uint64(len(b.requests))
	for {
		high := b.highWater.Load()
		if depth <= high || b.highWater.CompareAndSwap(high, depth) {
			return
		}
	}
}
