package node

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

type managedTLSSyncStatus string

const (
	managedTLSReady    managedTLSSyncStatus = "ready"
	managedTLSSyncing  managedTLSSyncStatus = "syncing"
	managedTLSWaiting  managedTLSSyncStatus = "waiting"
	managedTLSIssuing  managedTLSSyncStatus = "issuing"
	managedTLSDegraded managedTLSSyncStatus = "degraded"
	managedTLSBlocked  managedTLSSyncStatus = "blocked"
)

var ErrManagedTLSPending = errors.New("managed TLS certificate is not ready")

type managedTLSAPI interface {
	GetManagedTLSCertificate(context.Context, string, uint64) (*panel.ManagedTLSCertificate, error)
	LeaseManagedTLSCertificate(context.Context, panel.ManagedTLSLeaseRequest) (*panel.ManagedTLSLeaseResponse, error)
	PublishManagedTLSCertificate(context.Context, panel.ManagedTLSPublishRequest) error
	FailManagedTLSCertificate(context.Context, panel.ManagedTLSFailRequest) error
	TLSCertificateTokenConfigured() bool
	TLSCertificateTokenFingerprint() string
}

type managedTLSStatusSnapshot struct {
	Managed          bool
	ScopeID          uint64
	Version          uint64
	NotAfter         int64
	Status           managedTLSSyncStatus
	TokenConfigured  bool
	TokenFingerprint string
	LastErrorCode    string
	SyncRequestID    string
}

type managedTLSManager struct {
	client  managedTLSAPI
	store   managedTLSStore
	issuer  managedTLSIssuer
	scopeID uint64
	domain  string

	mu            sync.RWMutex
	snapshot      managedTLSStatusSnapshot
	cancel        context.CancelFunc
	done          chan struct{}
	started       bool
	closed        bool
	readyNotified bool
	now           func() time.Time
	jitter        func() time.Duration
}

func newManagedTLSManager(client managedTLSAPI, store managedTLSStore, issuer managedTLSIssuer, scopeID uint64, domain string) *managedTLSManager {
	tokenConfigured := client != nil && client.TLSCertificateTokenConfigured()
	tokenFingerprint := ""
	if client != nil {
		tokenFingerprint = client.TLSCertificateTokenFingerprint()
	}
	return &managedTLSManager{
		client: client, store: store, issuer: issuer, scopeID: scopeID,
		domain: strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), "."),
		snapshot: managedTLSStatusSnapshot{
			Managed: true, ScopeID: scopeID, Status: managedTLSBlocked,
			TokenConfigured: tokenConfigured, TokenFingerprint: tokenFingerprint,
		},
		now:    func() time.Time { return time.Now().UTC() },
		jitter: func() time.Duration { return time.Duration(rand.Intn(91)) * time.Second },
	}
}

func (m *managedTLSManager) Prepare(now time.Time) error {
	if err := m.validateConfiguration(); err != nil {
		m.setFailure(managedTLSBlocked, "managed_tls_config_invalid", nil)
		return ErrManagedTLSPending
	}
	local, localErr := m.store.Current(m.scopeID, m.domain, now)
	localValid := localErr == nil && local != nil
	if localValid {
		m.setFromLocal(managedTLSReady, local, "")
	}

	certificate, err := m.client.GetManagedTLSCertificate(context.Background(), m.domain, managedTLSLocalVersion(local))
	if err != nil {
		if localValid {
			m.setFromLocal(managedTLSDegraded, local, managedTLSErrorCode(err))
			return nil
		}
		status := managedTLSBlocked
		if managedTLSErrorCode(err) == "managed_tls_empty" {
			status = managedTLSWaiting
		}
		m.setFailure(status, managedTLSErrorCode(err), nil)
		return ErrManagedTLSPending
	}
	if certificate != nil && certificate.Status == "not_modified" {
		if localValid {
			m.setFromLocal(managedTLSReady, local, "")
			return nil
		}
		m.setFailure(managedTLSBlocked, "managed_tls_local_missing", nil)
		return ErrManagedTLSPending
	}
	if certificate == nil || certificate.Status != "ready" {
		if localValid {
			m.setFromLocal(managedTLSDegraded, local, "managed_tls_response_invalid")
			return nil
		}
		m.setFailure(managedTLSWaiting, "managed_tls_empty", nil)
		return ErrManagedTLSPending
	}
	if err := m.installPanelCertificate(certificate, now); err != nil {
		if localValid {
			m.setFromLocal(managedTLSDegraded, local, "managed_tls_certificate_invalid")
			return nil
		}
		m.setFailure(managedTLSBlocked, "managed_tls_certificate_invalid", nil)
		return ErrManagedTLSPending
	}
	return nil
}

func (m *managedTLSManager) Start(ctx context.Context, onReady func() error) {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return
	}
	loopContext, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	m.started = true
	done := m.done
	m.mu.Unlock()

	go func() {
		defer close(done)
		for {
			delay, _ := m.reconcileOnce(loopContext)
			if m.Snapshot().Status == managedTLSReady {
				m.notifyReady(loopContext, onReady)
			}
			if delay <= 0 {
				delay = time.Minute
			}
			timer := time.NewTimer(delay)
			select {
			case <-loopContext.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}()
}

func (m *managedTLSManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancel := m.cancel
	done := m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (m *managedTLSManager) Snapshot() managedTLSStatusSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot
}

func (m *managedTLSManager) MarkSyncRequestReported(requestID string) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	m.mu.Lock()
	if m.snapshot.SyncRequestID == requestID {
		m.snapshot.SyncRequestID = ""
	}
	m.mu.Unlock()
}

func (m *managedTLSManager) reconcileOnce(ctx context.Context) (time.Duration, error) {
	if err := m.validateConfiguration(); err != nil {
		m.setFailure(managedTLSBlocked, "managed_tls_config_invalid", nil)
		return time.Minute, ErrManagedTLSPending
	}
	now := m.now()
	local, localErr := m.store.Current(m.scopeID, m.domain, now)
	localValid := localErr == nil && local != nil
	certificate, getErr := m.client.GetManagedTLSCertificate(ctx, m.domain, managedTLSLocalVersion(local))
	if getErr != nil {
		code := managedTLSErrorCode(getErr)
		if code != "managed_tls_empty" {
			if localValid {
				m.setFromLocal(managedTLSDegraded, local, code)
				return fiveMinuteMaximum(time.Minute), nil
			}
			m.setFailure(managedTLSBlocked, code, nil)
			return time.Minute, getErr
		}
	} else if certificate != nil {
		switch certificate.Status {
		case "not_modified":
			if !localValid {
				m.setFailure(managedTLSBlocked, "managed_tls_local_missing", nil)
				return time.Minute, ErrManagedTLSPending
			}
			m.setFromLocal(managedTLSReady, local, "")
			return 10*time.Minute + m.jitter(), nil
		case "ready":
			needsInstall := !localValid || certificate.Version != local.Metadata.Version || certificate.ForceInstall
			if needsInstall {
				m.setSyncing(local)
				if err := m.installPanelCertificate(certificate, now); err != nil {
					if localValid {
						m.setFromLocal(managedTLSDegraded, local, "managed_tls_certificate_invalid")
						return time.Minute, nil
					}
					m.setFailure(managedTLSBlocked, "managed_tls_certificate_invalid", nil)
					return time.Minute, err
				}
				local, localErr = m.store.Current(m.scopeID, m.domain, now)
				localValid = localErr == nil && local != nil
			}
			if localValid && (certificate.RenewAt <= 0 || now.Unix() < certificate.RenewAt) {
				m.setFromLocal(managedTLSReady, local, "")
				if certificate.SyncRequestID != "" {
					m.setSyncRequestID(certificate.SyncRequestID)
				}
				return 10*time.Minute + m.jitter(), nil
			}
		default:
			if localValid {
				m.setFromLocal(managedTLSDegraded, local, "managed_tls_response_invalid")
				return time.Minute, nil
			}
		}
	}

	lease, err := m.client.LeaseManagedTLSCertificate(ctx, panel.ManagedTLSLeaseRequest{Domain: m.domain})
	if err != nil {
		if localValid {
			m.setFromLocal(managedTLSDegraded, local, managedTLSErrorCode(err))
		} else {
			m.setFailure(managedTLSBlocked, managedTLSErrorCode(err), nil)
		}
		return time.Minute, err
	}
	if lease == nil || (lease.ScopeID != 0 && lease.ScopeID != m.scopeID) {
		m.setFailure(managedTLSBlocked, "managed_tls_scope_mismatch", local)
		return time.Minute, ErrManagedTLSPending
	}
	switch lease.Status {
	case "ready":
		m.setWaiting(local, "")
		return clampManagedTLSRetry(lease.RetryAfter, 5), nil
	case "waiting", "cooldown":
		m.setWaiting(local, "")
		return clampManagedTLSRetry(lease.RetryAfter, 5), nil
	case "acquired":
		return m.issueWithLease(ctx, lease, local)
	default:
		m.setFailure(managedTLSBlocked, "managed_tls_lease_invalid", local)
		return time.Minute, ErrManagedTLSPending
	}
}

func (m *managedTLSManager) issueWithLease(ctx context.Context, lease *panel.ManagedTLSLeaseResponse, local *managedTLSLocalCertificate) (time.Duration, error) {
	if m.issuer == nil || strings.TrimSpace(lease.LeaseID) == "" {
		m.setFailure(managedTLSBlocked, "managed_tls_lease_invalid", local)
		return time.Minute, ErrManagedTLSPending
	}
	m.setIssuing(local)
	heartbeatContext, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatContext.Done():
				return
			case <-ticker.C:
				_, _ = m.client.LeaseManagedTLSCertificate(heartbeatContext, panel.ManagedTLSLeaseRequest{Domain: m.domain, LeaseID: lease.LeaseID})
			}
		}
	}()
	result, issueErr := m.issuer.Issue(ctx, managedTLSIssueRequest{
		ScopeID: m.scopeID, Domain: m.domain, Provider: lease.Provider,
		DNSEnv: lease.DNSEnv, ACMEAccount: lease.ACMEAccount,
	})
	cancelHeartbeat()
	<-heartbeatDone
	if issueErr != nil || result == nil {
		code := "managed_tls_obtain_failed"
		_ = m.client.FailManagedTLSCertificate(ctx, panel.ManagedTLSFailRequest{LeaseID: lease.LeaseID, Domain: m.domain, ErrorCode: code, Message: "managed TLS issuance failed"})
		m.setFailure(statusWithLocal(local), code, local)
		return time.Minute, issueErr
	}
	account := result.ACMEAccount
	publishRequest := panel.ManagedTLSPublishRequest{
		LeaseID: lease.LeaseID, Domain: m.domain,
		FullchainPEM: string(result.FullchainPEM), PrivateKeyPEM: string(result.PrivateKeyPEM),
		ACMEAccount: &account,
	}
	publishErr := m.client.PublishManagedTLSCertificate(ctx, publishRequest)
	issuedFingerprint, fingerprintErr := managedTLSCertificateFingerprint(result.FullchainPEM)
	canonical, getErr := m.client.GetManagedTLSCertificate(ctx, m.domain, 0)
	committed := getErr == nil && canonical != nil && canonical.Status == "ready" &&
		fingerprintErr == nil && strings.EqualFold(canonical.CertificateSHA256, issuedFingerprint)
	if publishErr != nil && !committed {
		code := managedTLSErrorCode(publishErr)
		_ = m.client.FailManagedTLSCertificate(ctx, panel.ManagedTLSFailRequest{LeaseID: lease.LeaseID, Domain: m.domain, ErrorCode: code, Message: "managed TLS publish failed"})
		m.setFailure(statusWithLocal(local), code, local)
		return time.Minute, publishErr
	}
	if !committed {
		code := managedTLSErrorCode(getErr)
		if code == "" {
			code = "managed_tls_publish_unconfirmed"
		}
		m.setFailure(statusWithLocal(local), code, local)
		return time.Minute, ErrManagedTLSPending
	}
	if err := m.installPanelCertificate(canonical, m.now()); err != nil {
		m.setFailure(statusWithLocal(local), "managed_tls_certificate_invalid", local)
		return time.Minute, err
	}
	return 10*time.Minute + m.jitter(), nil
}

func (m *managedTLSManager) installPanelCertificate(certificate *panel.ManagedTLSCertificate, now time.Time) error {
	if certificate == nil || certificate.Status != "ready" || certificate.ScopeID != m.scopeID || certificate.Version == 0 ||
		!sameManagedTLSDomain(certificate.Domain, m.domain) || certificate.NotAfter <= now.Unix() ||
		strings.TrimSpace(certificate.CertificateSHA256) == "" {
		return errManagedTLSLocalCertificateInvalid
	}
	local := managedTLSLocalCertificate{
		Metadata: managedTLSMetadata{
			ScopeID: m.scopeID, Domain: m.domain, Version: certificate.Version,
			CertificateSHA256: strings.ToLower(certificate.CertificateSHA256), NotAfter: certificate.NotAfter,
		},
		FullchainPEM: []byte(certificate.FullchainPEM), PrivateKeyPEM: []byte(certificate.PrivateKeyPEM),
	}
	if err := m.store.Install(local); err != nil {
		return err
	}
	installed, err := m.store.Current(m.scopeID, m.domain, now)
	if err != nil {
		return err
	}
	m.setFromLocal(managedTLSReady, installed, "")
	if certificate.SyncRequestID != "" {
		m.setSyncRequestID(certificate.SyncRequestID)
	}
	return nil
}

func (m *managedTLSManager) validateConfiguration() error {
	if m.client == nil || m.store == nil || m.scopeID == 0 || !validManagedTLSDomain(m.domain) || !m.client.TLSCertificateTokenConfigured() {
		return ErrManagedTLSPending
	}
	return nil
}

func (m *managedTLSManager) setFromLocal(status managedTLSSyncStatus, local *managedTLSLocalCertificate, errorCode string) {
	m.mu.Lock()
	m.snapshot.Status = status
	m.snapshot.LastErrorCode = sanitizeManagedTLSErrorCode(errorCode)
	if local != nil {
		m.snapshot.Version = local.Metadata.Version
		m.snapshot.NotAfter = local.Metadata.NotAfter
	}
	m.mu.Unlock()
}

func (m *managedTLSManager) setFailure(status managedTLSSyncStatus, code string, local *managedTLSLocalCertificate) {
	m.setFromLocal(status, local, code)
}

func (m *managedTLSManager) setWaiting(local *managedTLSLocalCertificate, code string) {
	m.setFromLocal(managedTLSWaiting, local, code)
}

func (m *managedTLSManager) setIssuing(local *managedTLSLocalCertificate) {
	m.setFromLocal(managedTLSIssuing, local, "")
}

func (m *managedTLSManager) setSyncing(local *managedTLSLocalCertificate) {
	m.setFromLocal(managedTLSSyncing, local, "")
}

func (m *managedTLSManager) setSyncRequestID(requestID string) {
	requestID = sanitizeManagedTLSErrorCode(requestID)
	m.mu.Lock()
	m.snapshot.SyncRequestID = requestID
	m.mu.Unlock()
}

func (m *managedTLSManager) notifyReady(ctx context.Context, callback func() error) {
	if callback == nil || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	if m.readyNotified || m.closed {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	if ctx.Err() != nil || callback() != nil {
		return
	}
	m.mu.Lock()
	if !m.closed {
		m.readyNotified = true
	}
	m.mu.Unlock()
}

func managedTLSLocalVersion(local *managedTLSLocalCertificate) uint64 {
	if local == nil {
		return 0
	}
	return local.Metadata.Version
}

func managedTLSErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var managedError *panel.ManagedTLSError
	if errors.As(err, &managedError) {
		return sanitizeManagedTLSErrorCode(managedError.Code)
	}
	return "managed_tls_transport_error"
}

func sanitizeManagedTLSErrorCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var builder strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '.' || char == ':' || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 64 {
			break
		}
	}
	return builder.String()
}

func clampManagedTLSRetry(seconds int, fallback int) time.Duration {
	if seconds <= 0 {
		seconds = fallback
	}
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func fiveMinuteMaximum(delay time.Duration) time.Duration {
	if delay <= 0 {
		return time.Minute
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func statusWithLocal(local *managedTLSLocalCertificate) managedTLSSyncStatus {
	if local != nil {
		return managedTLSDegraded
	}
	return managedTLSBlocked
}

func managedTLSCertificateFingerprint(fullchain []byte) (string, error) {
	block, _ := pem.Decode(fullchain)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("managed TLS certificate invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("managed TLS certificate invalid")
	}
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:]), nil
}
