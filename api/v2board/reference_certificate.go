package panel

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Fetch before accepting the configuration ETag so a failed download is retried.
// Versioned paths keep a working listener's certificate intact during rotation.
func (c *Client) referenceCertificate(ctx context.Context, revision, certificateDir string) (string, string, error) {
	raw, err := hex.DecodeString(revision)
	if err != nil || len(raw) != sha256.Size {
		return "", "", errors.New("invalid reference certificate revision")
	}
	host, err := url.Parse(c.APIHost)
	if err != nil || host.Scheme != "https" || host.Host == "" {
		return "", "", errors.New("reference certificate requires an HTTPS panel")
	}
	hostID := sha256.Sum256([]byte(normalizeManagedTLSAPIHost(c.APIHost)))
	base := filepath.Join(certificateDir, strconv.Itoa(c.NodeId), fmt.Sprintf("reference-%x", hostID[:8]))
	dest := filepath.Join(base, revision)
	chainPath, keyPath := filepath.Join(dest, "fullchain.pem"), filepath.Join(dest, "private.key")
	valid := func(chain, key []byte) bool {
		if fmt.Sprintf("%x", sha256.Sum256(chain)) != revision {
			return false
		}
		pair, err := tls.X509KeyPair(chain, key)
		if err != nil {
			return false
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		now := time.Now()
		return err == nil && !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter)
	}
	cached := func() bool {
		chain, err := os.ReadFile(chainPath)
		if err != nil {
			return false
		}
		key, err := os.ReadFile(keyPath)
		return err == nil && valid(chain, key)
	}
	if cached() {
		return chainPath, keyPath, nil
	}
	r, err := c.managedTLSClient.R().SetContext(ctx).SetAuthToken(c.Token).SetQueryParam("node_id", strconv.Itoa(c.NodeId)).Get("/api/v2/server/reference-certificate")
	if err != nil || r == nil {
		return "", "", errors.New("reference certificate download failed")
	}
	if r.StatusCode() != 200 {
		return "", "", fmt.Errorf("reference certificate HTTP %d", r.StatusCode())
	}
	var bundle struct {
		Chain string `json:"fullchain_pem"`
		Key   string `json:"private_key_pem"`
	}
	if json.Unmarshal(r.Body(), &bundle) != nil || !valid([]byte(bundle.Chain), []byte(bundle.Key)) {
		return "", "", errors.New("reference certificate revision, validity or key mismatch")
	}
	if err = os.MkdirAll(base, 0700); err != nil {
		return "", "", err
	}
	tmp, err := os.MkdirTemp(base, ".install-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(tmp)
	if err = os.WriteFile(filepath.Join(tmp, "private.key"), []byte(bundle.Key), 0600); err == nil {
		err = os.WriteFile(filepath.Join(tmp, "fullchain.pem"), []byte(bundle.Chain), 0600)
	}
	if err != nil {
		return "", "", err
	}
	if err = os.Rename(tmp, dest); err != nil && !cached() {
		return "", "", err
	}
	return chainPath, keyPath, nil
}
