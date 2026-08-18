package node

import (
	"context"
	"errors"
	"testing"
	"time"

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

type managedTLSDomainTestStore struct {
	certificate *managedTLSLocalCertificate
}

func (s *managedTLSDomainTestStore) Current(
	scopeID uint64,
	domain string,
	_ time.Time,
) (*managedTLSLocalCertificate, error) {
	if s.certificate == nil || s.certificate.Metadata.ScopeID != scopeID {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	domains, err := managedTLSMetadataDomains(s.certificate.Metadata)
	if err != nil || !managedTLSDomainsContain(domains, domain) {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	return s.certificate, nil
}

func (s *managedTLSDomainTestStore) Install(managedTLSLocalCertificate) error { return nil }
func (s *managedTLSDomainTestStore) Rollback() error                          { return nil }
func (s *managedTLSDomainTestStore) CertFile() string                         { return "fullchain.pem" }
func (s *managedTLSDomainTestStore) KeyFile() string                          { return "private.key" }

func TestManagedTLSManagerCommitsAuthoritativeDomainCoveredByCurrentCertificate(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store:            &managedTLSDomainTestStore{certificate: certificate},
		scopeID:          1,
		domain:           "old.example.com",
		activatedVersion: 2,
		snapshot:         managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}
	m.setFromLocal(managedTLSReady, certificate, "")

	persisted := false
	if err := m.CommitDomain("new.example.com", time.Unix(1_800_000_000, 0), func() error {
		persisted = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !persisted {
		t.Fatal("domain commit skipped persistence")
	}
	if got := m.Domain(); got != "new.example.com" {
		t.Fatalf("domain=%q want new.example.com", got)
	}
	if !m.Snapshot().MigrationPrepared {
		t.Fatal("same activated dual-SAN version must remain prepared")
	}
}

func TestManagedTLSManagerRejectsDomainMissingFromCurrentCertificateBeforePersistence(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store:    &managedTLSDomainTestStore{certificate: certificate},
		scopeID:  1,
		domain:   "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}

	persisted := false
	if err := m.CommitDomain("missing.example.com", time.Unix(1_800_000_000, 0), func() error {
		persisted = true
		return nil
	}); err == nil {
		t.Fatal("expected target SAN validation failure")
	}
	if persisted {
		t.Fatal("invalid target domain was persisted")
	}
	if got := m.Domain(); got != "old.example.com" {
		t.Fatalf("domain changed after rejection: %q", got)
	}
}

func TestManagedTLSManagerKeepsDomainWhenPersistenceFails(t *testing.T) {
	certificate := migrationCertificateForActivationTest(2)
	m := &managedTLSManager{
		store:    &managedTLSDomainTestStore{certificate: certificate},
		scopeID:  1,
		domain:   "old.example.com",
		snapshot: managedTLSStatusSnapshot{Managed: true, ScopeID: 1},
	}

	persistErr := errors.New("persist failed")
	err := m.CommitDomain("new.example.com", time.Unix(1_800_000_000, 0), func() error {
		return persistErr
	})
	if !errors.Is(err, persistErr) {
		t.Fatalf("err=%v want persist failure", err)
	}
	if got := m.Domain(); got != "old.example.com" {
		t.Fatalf("domain changed after persistence failure: %q", got)
	}
}

func managedTLSDomainSwitchNode(domain string) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:           302,
		Type:         "trojan",
		Security:     panel.Tls,
		PushInterval: time.Minute,
		PullInterval: time.Minute,
		Tag:          "node-302",
		Common: &panel.CommonNode{
			Protocol:   "trojan",
			ServerPort: 15014,
			BaseConfig: &panel.BaseConfig{},
			Tls:        panel.Tls,
			TlsSettings: panel.TlsSettings{
				CertMode:           "managed",
				CertificateScopeID: 1,
				ServerName:         domain,
				ServerNames:        []string{domain},
			},
			CertInfo: &panel.CertInfo{
				CertMode:   "managed",
				CertFile:   "fullchain.pem",
				KeyFile:    "private.key",
				CertDomain: domain,
			},
		},
	}
}

func TestManagedTLSDomainOnlyChange(t *testing.T) {
	current := managedTLSDomainSwitchNode("old.example.com")
	target := managedTLSDomainSwitchNode("new.example.com")
	domain, ok := managedTLSDomainOnlyChange(current, target)
	if !ok || domain != "new.example.com" {
		t.Fatalf("domain=%q ok=%v", domain, ok)
	}

	tests := []struct {
		name   string
		change func(*panel.NodeInfo)
	}{
		{name: "port", change: func(info *panel.NodeInfo) { info.Common.ServerPort++ }},
		{name: "scope", change: func(info *panel.NodeInfo) { info.Common.TlsSettings.CertificateScopeID++ }},
		{name: "protocol", change: func(info *panel.NodeInfo) { info.Common.Protocol = "vless" }},
		{name: "node id", change: func(info *panel.NodeInfo) { info.Id++ }},
		{name: "tag", change: func(info *panel.NodeInfo) { info.Tag = "other-tag" }},
		{name: "tls mode", change: func(info *panel.NodeInfo) { info.Common.TlsSettings.CertMode = "file" }},
		{name: "cert mode", change: func(info *panel.NodeInfo) { info.Common.CertInfo.CertMode = "file" }},
		{name: "reject unknown sni", change: func(info *panel.NodeInfo) { info.Common.CertInfo.RejectUnknownSni = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := managedTLSDomainSwitchNode("new.example.com")
			test.change(changed)
			if _, ok := managedTLSDomainOnlyChange(current, changed); ok {
				t.Fatal("non-domain change must retain the full reload path")
			}
		})
	}
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
	err := m.notifyReady(context.Background(), func() error {
		attempts++
		return errors.New("activate failed")
	})
	if err == nil {
		t.Fatal("failed activation must be returned to the reconcile loop")
	}
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
