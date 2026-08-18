package node

import (
	"context"
	"errors"
	"testing"
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
