package node

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wyx2685/v2node/common/file"
)

func (c *Controller) renewCertTask(_ context.Context) error {
	info := c.currentNodeInfo()
	if info == nil || info.Common == nil {
		return fmt.Errorf("node info is incomplete")
	}
	l, err := NewLego(info.Common.CertInfo)
	if err != nil {
		log.WithField("tag", c.tag).Info("new lego error: ", err)
		return nil
	}
	err = l.RenewCert()
	if err != nil {
		log.WithField("tag", c.tag).Info("renew cert error: ", err)
		return nil
	}
	return nil
}

func (c *Controller) requestCert() error {
	info := c.currentNodeInfo()
	if info == nil || info.Common == nil {
		return fmt.Errorf("node info is incomplete")
	}
	cert := info.Common.CertInfo
	if cert == nil {
		return &certificateError{code: "tls_certificate_config_missing", err: errors.New("missing certificate configuration")}
	}
	switch cert.CertMode {
	case "none", "":
	case "file":
		return validateCertificateFiles(cert.CertFile, cert.KeyFile)
	case "dns", "http":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path not exist")
		}
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		l, err := NewLego(cert)
		if err != nil {
			return fmt.Errorf("create lego object error: %s", err)
		}
		err = l.CreateCert()
		if err != nil {
			return fmt.Errorf("create lego cert error: %s", err)
		}
	case "self":
		if cert.CertFile == "" || cert.KeyFile == "" {
			return fmt.Errorf("cert file path or key file path not exist")
		}
		if file.IsExist(cert.CertFile) && file.IsExist(cert.KeyFile) {
			return nil
		}
		err := generateSelfSslCertificate(
			cert.CertDomain,
			cert.CertFile,
			cert.KeyFile)
		if err != nil {
			return fmt.Errorf("generate self cert error: %s", err)
		}
	default:
		return fmt.Errorf("unsupported certmode: %s", cert.CertMode)
	}
	return nil
}

type certificateError struct {
	code string
	err  error
}

func (e *certificateError) Error() string { return e.err.Error() }
func (e *certificateError) Unwrap() error { return e.err }

// File mode never creates or replaces operator-managed certificates.
func validateCertificateFiles(certPath, keyPath string) error {
	for _, entry := range []struct{ path, name, code string }{
		{certPath, "certificate", "tls_certificate_missing"},
		{keyPath, "private key", "tls_private_key_missing"},
	} {
		if entry.path == "" {
			return &certificateError{code: entry.code, err: fmt.Errorf("%s file path is empty", entry.name)}
		}
		stat, err := os.Stat(entry.path)
		if err != nil {
			code := "tls_certificate_unreadable"
			if errors.Is(err, os.ErrNotExist) {
				code = entry.code
			}
			return &certificateError{code: code, err: fmt.Errorf("%s file unavailable: %w", entry.name, err)}
		}
		if !stat.Mode().IsRegular() {
			return &certificateError{code: "tls_certificate_unreadable", err: fmt.Errorf("%s path is not a regular file: %s", entry.name, entry.path)}
		}
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		return &certificateError{code: "tls_key_pair_invalid", err: fmt.Errorf("certificate/private key pair is unreadable, invalid or mismatched: %w", err)}
	}
	return nil
}

func generateSelfSslCertificate(domain, certPath, keyPath string) error {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{
		Version:      3,
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject: pkix.Name{
			CommonName: domain,
		},
		DNSNames:              []string{domain},
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().AddDate(30, 0, 0),
	}
	cert, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(certPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	err = pem.Encode(f, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert,
	})
	if err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	f, err = os.OpenFile(keyPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	err = pem.Encode(f, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	if err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return nil
}

func certSha256Fingerprint(certPath string) (string, error) {
	data, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}

	remaining := data
	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(cert.Raw)
		return hex.EncodeToString(sum[:]), nil
	}

	cert, err := x509.ParseCertificate(data)
	if err == nil {
		sum := sha256.Sum256(cert.Raw)
		return hex.EncodeToString(sum[:]), nil
	}

	return "", fmt.Errorf("no certificate found in %s", certPath)
}
