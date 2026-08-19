package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns"
	"github.com/go-acme/lego/v4/registration"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

var managedTLSLegoMu sync.Mutex

var managedTLSCloudflareRecursiveNameservers = []string{
	"1.1.1.1:53",
	"8.8.8.8:53",
}

type managedTLSDNS01Policy struct {
	RecursiveNameservers []string
	PropagationWait      time.Duration
	SkipPropagationCheck bool
}

func managedTLSDNS01PolicyForProvider(provider string) (managedTLSDNS01Policy, bool) {
	if !strings.EqualFold(strings.TrimSpace(provider), "cloudflare") {
		return managedTLSDNS01Policy{}, false
	}

	return managedTLSDNS01Policy{
		RecursiveNameservers: append([]string(nil), managedTLSCloudflareRecursiveNameservers...),
		PropagationWait:      15 * time.Second,
		SkipPropagationCheck: true,
	}, true
}

func managedTLSDNS01ChallengeOptions(provider string) []dns01.ChallengeOption {
	policy, ok := managedTLSDNS01PolicyForProvider(provider)
	if !ok {
		return nil
	}

	return []dns01.ChallengeOption{
		dns01.AddRecursiveNameservers(policy.RecursiveNameservers),
		dns01.PropagationWait(policy.PropagationWait, policy.SkipPropagationCheck),
	}
}

type managedTLSIssueRequest struct {
	ScopeID     uint64
	Domains     []string
	Provider    string
	DNSEnv      map[string]string
	ACMEAccount *panel.ManagedTLSACMEAccount
}

type managedTLSIssueResult struct {
	FullchainPEM  []byte
	PrivateKeyPEM []byte
	ACMEAccount   panel.ManagedTLSACMEAccount
}

type managedTLSIssuer interface {
	Issue(ctx context.Context, request managedTLSIssueRequest) (*managedTLSIssueResult, error)
}

type managedTLSLegoIssuer struct {
	obtain func(context.Context, managedTLSIssueRequest) (*managedTLSIssueResult, error)
}

func (i *managedTLSLegoIssuer) Issue(ctx context.Context, request managedTLSIssueRequest) (*managedTLSIssueResult, error) {
	managedTLSLegoMu.Lock()
	defer managedTLSLegoMu.Unlock()
	domains, err := normalizeManagedTLSDomains(request.Domains)
	if err != nil {
		return nil, fmt.Errorf("managed_tls_issue_config_invalid")
	}
	request.Domains = domains

	restore, err := applyManagedTLSDNSEnvironment(request.DNSEnv)
	if err != nil {
		return nil, fmt.Errorf("managed_tls_dns_environment_invalid")
	}
	defer restore()
	if i.obtain != nil {
		return i.obtain(ctx, request)
	}
	return issueManagedTLSWithLego(ctx, request)
}

func issueManagedTLSWithLego(ctx context.Context, request managedTLSIssueRequest) (*managedTLSIssueResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("managed_tls_issue_cancelled")
	}
	domains, domainErr := normalizeManagedTLSDomains(request.Domains)
	provider := strings.ToLower(strings.TrimSpace(request.Provider))
	if domainErr != nil || provider == "" || len(request.DNSEnv) == 0 {
		return nil, fmt.Errorf("managed_tls_issue_config_invalid")
	}

	var user *User
	var err error
	if request.ACMEAccount != nil {
		user, err = managedTLSUserFromAccount(request.ACMEAccount)
	} else {
		user, err = newManagedTLSLegoUser("node@v2board.com")
	}
	if err != nil {
		return nil, fmt.Errorf("managed_tls_acme_account_invalid")
	}
	config := lego.NewConfig(user)
	config.Certificate.KeyType = certcrypto.RSA2048
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, fmt.Errorf("managed_tls_lego_client_failed")
	}
	if user.Registration == nil {
		user.Registration, err = client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("managed_tls_acme_registration_failed")
		}
	}
	providerInstance, err := dns.NewDNSChallengeProviderByName(provider)
	if err != nil {
		return nil, fmt.Errorf("managed_tls_dns_provider_failed")
	}
	if err := client.Challenge.SetDNS01Provider(
		providerInstance,
		managedTLSDNS01ChallengeOptions(provider)...,
	); err != nil {
		return nil, fmt.Errorf("managed_tls_dns_provider_failed")
	}
	resource, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: domains, Bundle: true})
	if err != nil {
		return nil, managedTLSObtainFailure(request.ScopeID, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("managed_tls_issue_cancelled")
	}
	account, err := exportManagedTLSAccount(user)
	if err != nil {
		return nil, fmt.Errorf("managed_tls_acme_account_invalid")
	}
	return &managedTLSIssueResult{
		FullchainPEM:  append([]byte(nil), resource.Certificate...),
		PrivateKeyPEM: append([]byte(nil), resource.PrivateKey...),
		ACMEAccount:   *account,
	}, nil
}

func managedTLSObtainFailure(scopeID uint64, cause error) error {
	log.WithError(cause).
		WithField("scope_id", scopeID).
		Warn("Managed TLS certificate obtain failed")

	return fmt.Errorf("managed_tls_obtain_failed")
}

func normalizeManagedTLSDomains(domains []string) ([]string, error) {
	seen := make(map[string]struct{}, len(domains))
	normalized := make([]string, 0, len(domains))
	for _, value := range domains {
		domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
		if !validManagedTLSDomain(domain) {
			return nil, fmt.Errorf("managed TLS domain invalid")
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		normalized = append(normalized, domain)
	}
	if len(normalized) < 1 || len(normalized) > 2 {
		return nil, fmt.Errorf("managed TLS domain count invalid")
	}
	sort.Strings(normalized)
	return normalized, nil
}

func newManagedTLSLegoUser(email string) (*User, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &User{Email: email, key: privateKey}, nil
}

func applyManagedTLSDNSEnvironment(environment map[string]string) (func(), error) {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type oldValue struct {
		value  string
		exists bool
	}
	old := make(map[string]oldValue, len(keys))
	applied := make([]string, 0, len(keys))
	restore := func() {
		for index := len(applied) - 1; index >= 0; index-- {
			key := applied[index]
			previous := old[key]
			if previous.exists {
				_ = os.Setenv(key, previous.value)
			} else {
				_ = os.Unsetenv(key)
			}
		}
	}
	for _, key := range keys {
		if strings.TrimSpace(key) == "" || strings.ContainsRune(key, '=') {
			restore()
			return nil, fmt.Errorf("invalid environment key")
		}
		value, exists := os.LookupEnv(key)
		old[key] = oldValue{value: value, exists: exists}
		if err := os.Setenv(key, environment[key]); err != nil {
			restore()
			return nil, err
		}
		applied = append(applied, key)
	}
	return restore, nil
}
