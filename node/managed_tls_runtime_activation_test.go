package node

import (
	"context"
	"errors"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func migrationCertificateForActivationTest(version uint64) *managedTLSLocalCertificate {
	return &managedTLSLocalCertificate{Metadata: managedTLSMetadata{
		ScopeID:     1,
		Domain:      "old.example.com",
		Domains:     []string{"new.example.com", "old.example.com"},
		MigrationID: 9,
		Version:     version,
		NotAfter:    2_000_000_000,
	}}
}

func TestManagedTLSReadyNotificationTracksActivatedVersion(t *testing.T) {
	m := &managedTLSManager{
		domain:   "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true},
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(1), "")
	if m.Snapshot().MigrationPrepared {
		t.Fatal("certificate must not be prepared before runtime activation")
	}

	calls := 0
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	if !m.Snapshot().MigrationPrepared {
		t.Fatal("certificate must be prepared after runtime activation")
	}

	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(2), "")
	if m.Snapshot().MigrationPrepared {
		t.Fatal("new installed version must wait for runtime activation")
	}
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	m.notifyReady(context.Background(), func() error {
		calls++
		return nil
	})
	if calls != 2 {
		t.Fatalf("activation callbacks = %d, want 2", calls)
	}
}

func TestManagedTLSReadyNotificationRetriesFailedActivation(t *testing.T) {
	m := &managedTLSManager{
		domain:   "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true},
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(3), "")

	attempts := 0
	m.notifyReady(context.Background(), func() error {
		attempts++
		return errors.New("activate failed")
	})
	if m.Snapshot().MigrationPrepared {
		t.Fatal("failed activation must keep migration gate closed")
	}
	m.setFromLocal(managedTLSReady, migrationCertificateForActivationTest(3), "")
	m.notifyReady(context.Background(), func() error {
		attempts++
		return nil
	})
	if attempts != 2 || !m.Snapshot().MigrationPrepared {
		t.Fatalf("attempts=%d prepared=%v", attempts, m.Snapshot().MigrationPrepared)
	}
}

func TestControllerActivatesOnlyChangedManagedTLSVersion(t *testing.T) {
	info := &panel.NodeInfo{Tag: "node-302"}
	users := []panel.UserInfo{{Id: 1, Uuid: "user-1"}}
	c := &Controller{
		info:                     info,
		tag:                      info.Tag,
		userList:                 append([]panel.UserInfo(nil), users...),
		managedTLSRuntimeVersion: 1,
	}
	c.runtime.started = true

	calls := 0
	c.replaceManagedTLSInbound = func(
		tag string,
		gotInfo *panel.NodeInfo,
		gotUsers []panel.UserInfo,
	) error {
		calls++
		if tag != "node-302" || gotInfo != info || len(gotUsers) != 1 || gotUsers[0].Uuid != "user-1" {
			t.Fatalf("unexpected replacement input: tag=%s users=%v", tag, gotUsers)
		}
		return nil
	}

	if err := c.activateManagedTLSRuntimeVersion(2); err != nil {
		t.Fatal(err)
	}
	if err := c.activateManagedTLSRuntimeVersion(2); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.managedTLSRuntimeVersion != 2 {
		t.Fatalf("calls=%d active=%d", calls, c.managedTLSRuntimeVersion)
	}
}

func TestControllerKeepsPreviousVersionWhenReplacementFails(t *testing.T) {
	c := &Controller{
		info:                     &panel.NodeInfo{Tag: "node-302"},
		tag:                      "node-302",
		managedTLSRuntimeVersion: 4,
		replaceManagedTLSInbound: func(string, *panel.NodeInfo, []panel.UserInfo) error {
			return errors.New("replace failed")
		},
	}
	c.runtime.started = true
	if err := c.activateManagedTLSRuntimeVersion(5); err == nil {
		t.Fatal("expected replacement failure")
	}
	if c.managedTLSRuntimeVersion != 4 {
		t.Fatalf("active version=%d want=4", c.managedTLSRuntimeVersion)
	}
}
