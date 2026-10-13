package sntpv3

import (
	"os"
	"sync"
	"time"
)

// Budget is shared by all logical sessions of one node, independently per
// direction. It accounts retained payload, not TLS/WS or Go allocator overhead.
type Budget struct {
	mu          sync.Mutex
	limit, used int
	changed     chan struct{}
}

func NewBudget(limit int) *Budget { return &Budget{limit: limit, changed: make(chan struct{})} }
func (b *Budget) acquire(amount int) (int, <-chan struct{}) {
	if b == nil {
		return amount, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	n := minInt(amount, b.limit-b.used)
	b.used += n
	return n, b.changed
}
func (b *Budget) release(amount int) {
	if b == nil || amount == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used -= amount
	if b.used < 0 {
		panic("V3 buffer accounting underflow")
	}
	close(b.changed)
	b.changed = make(chan struct{})
}
func waitBudget(ch, budget <-chan struct{}, deadline time.Time) error {
	var timeout <-chan time.Time
	if !deadline.IsZero() {
		duration := time.Until(deadline)
		if duration < 0 {
			duration = 0
		}
		t := time.NewTimer(duration)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ch:
		return nil
	case <-budget:
		return nil
	case <-timeout:
		return os.ErrDeadlineExceeded
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
