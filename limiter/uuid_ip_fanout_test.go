package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
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

func itoa(value int) string {
	if value < 10 {
		return string(rune('0' + value))
	}
	return string([]byte{'0' + byte(value/10), '0' + byte(value%10)})
}
