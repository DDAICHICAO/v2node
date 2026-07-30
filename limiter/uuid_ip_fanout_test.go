package limiter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUUIDIPFanoutRejectsOnlyNewIPBeyondThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := NewUUIDIPFanoutTracker(UUIDIPFanoutConfig{
		Enabled:       true,
		Mode:          "reject",
		Window:        10 * time.Minute,
		MaxUniqueIPs:  2,
		EventCooldown: time.Minute,
	})

	if got := tracker.Check("tag|device-a", 7, "192.0.2.1", now, false); got.Reject {
		t.Fatalf("first IP rejected: %+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "::ffff:192.0.2.2", now, false); got.Reject {
		t.Fatalf("second IP rejected: %+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "192.0.2.3", now, false); !got.Reject || got.Scope != "local" || got.UniqueIPCount != 3 {
		t.Fatalf("third IP decision=%+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "192.0.2.3", now.Add(time.Second), false); !got.Reject {
		t.Fatalf("rejected candidate was incorrectly stored: %+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "192.0.2.1", now.Add(time.Second), false); got.Reject {
		t.Fatalf("existing allowed IP rejected: %+v", got)
	}
}

func TestUUIDIPFanoutAuditExpiryExemptAndCIDR(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := NewUUIDIPFanoutTracker(UUIDIPFanoutConfig{
		Enabled:        true,
		Mode:           "audit",
		Window:         time.Minute,
		MaxUniqueIPs:   2,
		EventCooldown:  time.Minute,
		WhitelistCIDRs: []string{"198.51.100.0/24", "2001:db8::/32"},
	})

	tracker.Check("tag|device-a", 7, "192.0.2.1", now, false)
	tracker.Check("tag|device-a", 7, "192.0.2.2", now, false)
	if got := tracker.Check("tag|device-a", 7, "192.0.2.3", now, false); got.Reject || !got.EventGenerated {
		t.Fatalf("audit candidate decision=%+v", got)
	}
	if events := tracker.DrainEvents(500); len(events) != 1 || events[0].Action != "audit" {
		t.Fatalf("audit events=%+v", events)
	}
	if got := tracker.Check("tag|device-b", 8, "192.0.2.4", now, true); got.EventGenerated || got.Reject {
		t.Fatalf("exempt decision=%+v", got)
	}
	if got := tracker.Check("tag|device-c", 9, "198.51.100.8", now, false); got.EventGenerated || got.Reject {
		t.Fatalf("CIDR decision=%+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "192.0.2.4", now.Add(61*time.Second), false); got.Reject || got.UniqueIPCount != 1 {
		t.Fatalf("expired window decision=%+v", got)
	}
}

func TestUUIDIPFanoutGlobalDecisionAndRevision(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := NewUUIDIPFanoutTracker(UUIDIPFanoutConfig{
		Enabled:      true,
		Mode:         "reject",
		Window:       10 * time.Minute,
		MaxUniqueIPs: 10,
	})
	allowed := sha256.Sum256([]byte("192.0.2.1"))
	tracker.UpdateGlobal(UUIDIPFanoutGlobalState{
		Revision: 12,
		Mode:     "reject",
		States: []UUIDIPFanoutGlobalDecision{{
			UserID:            7,
			UUID:              "device-a",
			AllowedIPHashes:   []string{hex.EncodeToString(allowed[:])},
			DecisionExpiresAt: now.Add(time.Minute).Unix(),
		}},
	}, now)

	if got := tracker.Check("tag|device-a", 7, "192.0.2.1", now, false); got.Reject {
		t.Fatalf("globally allowed IP rejected: %+v", got)
	}
	if got := tracker.Check("tag|device-a", 7, "192.0.2.2", now, false); !got.Reject || got.Scope != "global" {
		t.Fatalf("global reject decision=%+v", got)
	}

	tracker.UpdateGlobal(UUIDIPFanoutGlobalState{Revision: 11, Mode: "reject"}, now)
	if got := tracker.Check("tag|device-a", 7, "192.0.2.2", now, false); !got.Reject {
		t.Fatalf("stale revision replaced global state: %+v", got)
	}
	tracker.UpdateGlobal(UUIDIPFanoutGlobalState{Revision: 13, Mode: "audit"}, now)
	if got := tracker.Check("tag|device-a", 7, "192.0.2.2", now, false); got.Reject {
		t.Fatalf("audit global state rejected: %+v", got)
	}
}

func TestUUIDIPFanoutDeleteAndConcurrentThreshold(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := NewUUIDIPFanoutTracker(UUIDIPFanoutConfig{
		Enabled:      true,
		Mode:         "reject",
		Window:       10 * time.Minute,
		MaxUniqueIPs: 10,
	})

	var wg sync.WaitGroup
	var accepted int
	var mu sync.Mutex
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func(lastOctet int) {
			defer wg.Done()
			got := tracker.Check("tag|device-a", 7, "192.0.2."+itoa(lastOctet), now, false)
			if !got.Reject {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted != 10 {
		t.Fatalf("accepted=%d, want 10", accepted)
	}

	tracker.Delete("tag|device-a")
	if got := tracker.Check("tag|device-a", 7, "192.0.2.32", now, false); got.Reject || got.UniqueIPCount != 1 {
		t.Fatalf("deleted window was retained: %+v", got)
	}
}

func TestUUIDIPFanoutReservationExistingIPAndAllowCommitAreCallFree(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newReservationTracker()
	inspection := tracker.Inspect("tag|device-a", 7, "192.0.2.1", now, false)
	if inspection.Kind != UUIDIPFanoutNeedsReservation {
		t.Fatalf("inspection=%+v", inspection)
	}
	if got := tracker.CommitAllowed(inspection, now, false); got.Reject {
		t.Fatalf("commit=%+v", got)
	}

	var calls atomic.Int32
	tracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		calls.Add(1)
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeAllow}
	})
	if got := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.1", now.Add(time.Second), false); got.Reject {
		t.Fatalf("existing=%+v", got)
	}
	if calls.Load() != 0 {
		t.Fatalf("reservation calls=%d", calls.Load())
	}

	if got := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.2", now.Add(time.Second), false); got.Reject {
		t.Fatalf("allow=%+v", got)
	}
	if got := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.2", now.Add(2*time.Second), false); got.Reject {
		t.Fatalf("committed existing=%+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("reservation calls=%d", calls.Load())
	}
}

func TestUUIDIPFanoutReservationSingleflightAndRejectNotCommitted(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newReservationTracker()
	var calls atomic.Int32
	release := make(chan struct{})
	tracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		calls.Add(1)
		<-release
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeAllow}
	})

	const workers = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan UUIDIPFanoutCheckResult, workers)
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- tracker.CheckWithReservation(
				context.Background(),
				"tag|device-a",
				7,
				"192.0.2.1",
				now,
				false,
			)
		}()
	}
	close(start)
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for result := range results {
		if result.Reject {
			t.Fatalf("singleflight result=%+v", result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("reservation calls=%d", calls.Load())
	}

	rejectTracker := newReservationTracker()
	var rejectCalls atomic.Int32
	rejectTracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		rejectCalls.Add(1)
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeReject}
	})
	if got := rejectTracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.9", now, false); !got.Reject {
		t.Fatalf("reject=%+v", got)
	}
	if got := rejectTracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.9", now.Add(time.Second), false); !got.Reject {
		t.Fatalf("repeated reject=%+v", got)
	}
	if rejectCalls.Load() != 2 {
		t.Fatalf("rejected candidate was committed, calls=%d", rejectCalls.Load())
	}
}

func TestUUIDIPFanoutReservationTemporaryFailureAndBreaker(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newReservationTracker()
	var calls atomic.Int32
	tracker.SetReservationFunc(func(ctx context.Context, _ UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		calls.Add(1)
		<-ctx.Done()
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFailOpen}
	})

	started := time.Now()
	result := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.1", now, false)
	if result.Reject || result.Scope != UUIDIPFanoutOutcomeFailOpen {
		t.Fatalf("fail-open=%+v", result)
	}
	if elapsed := time.Since(started); elapsed > 900*time.Millisecond {
		t.Fatalf("fail-open elapsed=%s", elapsed)
	}
	events := tracker.DrainEvents(10)
	if len(events) != 1 || events[0].Scope != "reservation_fail_open" {
		t.Fatalf("compensation events=%+v", events)
	}

	for index := 2; index <= 3; index++ {
		ip := "192.0.2." + itoa(index)
		if got := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, ip, now, false); got.Reject {
			t.Fatalf("temporary failure %d=%+v", index, got)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls before breaker=%d", calls.Load())
	}
	if got := tracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.4", now, false); got.Reject {
		t.Fatalf("open breaker=%+v", got)
	}
	if calls.Load() != 3 {
		t.Fatalf("open breaker called reserver, calls=%d", calls.Load())
	}
}

func TestUUIDIPFanoutReservationHalfOpenAllowsOneProbe(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tracker := newReservationTracker()
	tracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFailOpen}
	})
	for index := 1; index <= 3; index++ {
		tracker.CheckWithReservation(
			context.Background(),
			"tag|device-a",
			7,
			"192.0.2."+itoa(index),
			now,
			false,
		)
	}

	var calls atomic.Int32
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	tracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		calls.Add(1)
		close(probeStarted)
		<-releaseProbe
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeAllow}
	})
	halfOpenNow := now.Add(31 * time.Second)
	done := make(chan UUIDIPFanoutCheckResult, 1)
	go func() {
		done <- tracker.CheckWithReservation(
			context.Background(),
			"tag|device-a",
			7,
			"192.0.2.10",
			halfOpenNow,
			false,
		)
	}()
	<-probeStarted
	concurrent := tracker.CheckWithReservation(
		context.Background(),
		"tag|device-a",
		7,
		"192.0.2.11",
		halfOpenNow,
		false,
	)
	if concurrent.Reject || concurrent.Scope != UUIDIPFanoutOutcomeFailOpen {
		t.Fatalf("concurrent half-open=%+v", concurrent)
	}
	close(releaseProbe)
	if result := <-done; result.Reject {
		t.Fatalf("probe=%+v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("half-open calls=%d", calls.Load())
	}
}

func TestUUIDIPFanoutReservationProtocolFallbackAndReentry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	protocolTracker := newReservationTracker()
	protocolTracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeProtocol}
	})
	if got := protocolTracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.1", now, false); !got.Reject || got.Scope != "protocol" {
		t.Fatalf("protocol=%+v", got)
	}

	fallbackTracker := newReservationTracker()
	allowed := sha256.Sum256([]byte("192.0.2.1"))
	fallbackTracker.UpdateGlobal(UUIDIPFanoutGlobalState{
		Revision: 1,
		Mode:     "reject",
		States: []UUIDIPFanoutGlobalDecision{{
			UserID:            7,
			UUID:              "device-a",
			AllowedIPHashes:   []string{hex.EncodeToString(allowed[:])},
			DecisionExpiresAt: now.Add(time.Minute).Unix(),
		}},
	}, now)
	fallbackTracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFallback}
	})
	if got := fallbackTracker.CheckWithReservation(context.Background(), "tag|device-a", 7, "192.0.2.2", now, false); !got.Reject || got.Scope != "global" {
		t.Fatalf("fallback=%+v", got)
	}
	fallbackTracker.reservation.mu.Lock()
	failures := fallbackTracker.reservation.failures
	fallbackTracker.reservation.mu.Unlock()
	if failures != 0 {
		t.Fatalf("fallback breaker failures=%d", failures)
	}

	reentryTracker := newReservationTracker()
	reentryTracker.SetReservationFunc(func(context.Context, UUIDIPFanoutInspection) UUIDIPFanoutReservationOutcome {
		inspection := reentryTracker.Inspect("tag|device-b", 8, "192.0.2.8", now, true)
		if inspection.Kind != UUIDIPFanoutBypass {
			t.Fatalf("reentry inspection=%+v", inspection)
		}
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeAllow}
	})
	done := make(chan UUIDIPFanoutCheckResult, 1)
	go func() {
		done <- reentryTracker.CheckWithReservation(
			context.Background(),
			"tag|device-a",
			7,
			"192.0.2.7",
			now,
			false,
		)
	}()
	select {
	case result := <-done:
		if result.Reject {
			t.Fatalf("reentry result=%+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("reservation callback deadlocked on tracker mutex")
	}
}

func newReservationTracker() *UUIDIPFanoutTracker {
	return NewUUIDIPFanoutTracker(UUIDIPFanoutConfig{
		Enabled:            true,
		Mode:               "reject",
		Window:             10 * time.Minute,
		MaxUniqueIPs:       20,
		EventCooldown:      time.Minute,
		Strategy:           "reservation",
		ReservationEnabled: true,
		ReservationTimeout: 800 * time.Millisecond,
	})
}

func itoa(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}
