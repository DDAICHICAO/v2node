package node

import (
	"fmt"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

func TestOfflineTrackerEmitsThresholdsOnceAndResets(t *testing.T) {
	var tracker offlineTracker
	start := time.Unix(1_700_000_000, 0)
	if got := tracker.Failure("sync", start); !got.Entered {
		t.Fatalf("first failure did not enter offline mode: %+v", got)
	}
	tracker.Failure("report", start.Add(time.Hour))
	if got := tracker.Failure("sync", start.Add(24*time.Hour)); !got.Warn24h || got.Warn72h {
		t.Fatalf("24h transition=%+v", got)
	}
	if got := tracker.Failure("sync", start.Add(72*time.Hour)); got.Warn24h || !got.Warn72h {
		t.Fatalf("72h transition=%+v", got)
	}
	if got := tracker.Failure("sync", start.Add(96*time.Hour)); got.Entered || got.Warn24h || got.Warn72h {
		t.Fatalf("repeated failure should be quiet: %+v", got)
	}
	if _, recovered := tracker.Success("sync", start.Add(97*time.Hour)); recovered {
		t.Fatal("sync recovery must wait for report component")
	}
	duration, recovered := tracker.Success("report", start.Add(97*time.Hour))
	if !recovered || duration != 97*time.Hour {
		t.Fatalf("recovery duration=%v recovered=%v", duration, recovered)
	}
	if got := tracker.Failure("sync", start.Add(98*time.Hour)); !got.Entered {
		t.Fatal("tracker did not reset after recovery")
	}
}

func TestBootstrapAuthorizationFailureHasSpecificConfigStatus(t *testing.T) {
	cfg := conf.NodeConfig{APIHost: "https://panel.example", NodeID: 7}
	for _, status := range []int{401, 403} {
		err := newConfigError(cfg, "bootstrap", "node_bootstrap_failed", fmt.Errorf("bootstrap: %w", &panel.ValidationError{Operation: "get node info", StatusCode: status, Message: "node authorization rejected"}))
		rows := configStatusReports([]conf.NodeConfig{cfg}, nil, err, time.Unix(1000, 0))
		if len(rows) != 1 || rows[0].State != "failed" || rows[0].Stage != "authentication" || rows[0].ErrorCode != "node_authorization_rejected" || rows[0].RuntimeServing {
			t.Fatalf("authorization failure lost attribution: %+v", rows)
		}
	}
	if got := credentialMigrationCode(fmt.Errorf("credential_identity_rejected")); got != "credential_identity_rejected" {
		t.Fatalf("migration report lost identity rejection: %s", got)
	}
}
