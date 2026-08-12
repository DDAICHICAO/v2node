package node

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errManagedTLSLocalCertificateInvalid = errors.New("managed TLS local certificate invalid")

type managedTLSMetadata struct {
	ScopeID           uint64 `json:"scope_id"`
	Domain            string `json:"domain"`
	Version           uint64 `json:"version"`
	CertificateSHA256 string `json:"certificate_sha256"`
	NotAfter          int64  `json:"not_after"`
}

type managedTLSLocalCertificate struct {
	Metadata      managedTLSMetadata
	FullchainPEM  []byte
	PrivateKeyPEM []byte
	NotBefore     time.Time
	NotAfter      time.Time
}

type managedTLSStore interface {
	Current(expectedScopeID uint64, domain string, now time.Time) (*managedTLSLocalCertificate, error)
	Install(cert managedTLSLocalCertificate) error
	Rollback() error
	CertFile() string
	KeyFile() string
}

type managedTLSFileStore struct {
	root string
}

func newManagedTLSFileStore(base string, nodeID int) *managedTLSFileStore {
	return &managedTLSFileStore{root: filepath.Join(base, strconv.Itoa(nodeID))}
}

func (s *managedTLSFileStore) CertFile() string {
	return filepath.Join(s.root, "current", "fullchain.pem")
}

func (s *managedTLSFileStore) KeyFile() string {
	return filepath.Join(s.root, "current", "private.key")
}

func (s *managedTLSFileStore) Current(expectedScopeID uint64, domain string, now time.Time) (*managedTLSLocalCertificate, error) {
	certificate, err := readManagedTLSCertificate(filepath.Join(s.root, "current"))
	if err != nil {
		return nil, err
	}
	if certificate.Metadata.ScopeID != expectedScopeID || !sameManagedTLSDomain(certificate.Metadata.Domain, domain) {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	if err := validateManagedTLSLocalCertificate(certificate, now, true); err != nil {
		return nil, err
	}
	return certificate, nil
}

func (s *managedTLSFileStore) Install(certificate managedTLSLocalCertificate) error {
	if certificate.Metadata.ScopeID == 0 || certificate.Metadata.Version == 0 {
		return errManagedTLSLocalCertificateInvalid
	}
	if err := validateManagedTLSLocalCertificate(&certificate, time.Now(), true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.root, "versions"), 0o700); err != nil {
		return fmt.Errorf("create managed TLS versions directory: %w", err)
	}
	if err := os.Chmod(s.root, 0o700); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("secure managed TLS node directory: %w", err)
	}
	versionName := fmt.Sprintf("scope-%d-v%d", certificate.Metadata.ScopeID, certificate.Metadata.Version)
	finalDir := filepath.Join(s.root, "versions", versionName)
	if existing, err := readManagedTLSCertificate(finalDir); err == nil {
		if !sameManagedTLSCertificate(existing, &certificate) {
			return errManagedTLSLocalCertificateInvalid
		}
		return s.switchCurrent(versionName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tempDir, err := os.MkdirTemp(filepath.Join(s.root, "versions"), "."+versionName+".tmp-")
	if err != nil {
		return fmt.Errorf("create managed TLS temporary version: %w", err)
	}
	defer os.RemoveAll(tempDir)
	if err := os.Chmod(tempDir, 0o700); err != nil && runtime.GOOS != "windows" {
		return err
	}
	metadata, err := json.Marshal(certificate.Metadata)
	if err != nil {
		return errManagedTLSLocalCertificateInvalid
	}
	if err := writeManagedTLSFile(filepath.Join(tempDir, "metadata.json"), append(metadata, '\n'), 0o600); err != nil {
		return err
	}
	if err := writeManagedTLSFile(filepath.Join(tempDir, "fullchain.pem"), certificate.FullchainPEM, 0o644); err != nil {
		return err
	}
	if err := writeManagedTLSFile(filepath.Join(tempDir, "private.key"), certificate.PrivateKeyPEM, 0o600); err != nil {
		return err
	}
	if installed, err := readManagedTLSCertificate(tempDir); err != nil || !sameManagedTLSCertificate(installed, &certificate) {
		return errManagedTLSLocalCertificateInvalid
	}
	if err := syncManagedTLSDirectory(tempDir); err != nil {
		return err
	}
	if err := os.Rename(tempDir, finalDir); err != nil {
		if existing, readErr := readManagedTLSCertificate(finalDir); readErr != nil || !sameManagedTLSCertificate(existing, &certificate) {
			return fmt.Errorf("publish managed TLS version: %w", err)
		}
	}
	if err := syncManagedTLSDirectory(filepath.Join(s.root, "versions")); err != nil {
		return err
	}
	if err := s.switchCurrent(versionName); err != nil {
		return err
	}
	return s.cleanupVersions()
}

func (s *managedTLSFileStore) Rollback() error {
	if runtime.GOOS == "windows" {
		current := filepath.Join(s.root, "current")
		previous := filepath.Join(s.root, "previous")
		if _, err := os.Stat(previous); err != nil {
			return err
		}
		temp := filepath.Join(s.root, ".rollback-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if err := os.Rename(current, temp); err != nil {
			return err
		}
		if err := os.Rename(previous, current); err != nil {
			_ = os.Rename(temp, current)
			return err
		}
		if err := os.Rename(temp, previous); err != nil {
			return err
		}
		return syncManagedTLSDirectory(s.root)
	}

	currentTarget, err := os.Readlink(filepath.Join(s.root, "current"))
	if err != nil {
		return err
	}
	previousTarget, err := os.Readlink(filepath.Join(s.root, "previous"))
	if err != nil {
		return err
	}
	if err := replaceManagedTLSSymlink(s.root, "current", previousTarget); err != nil {
		return err
	}
	if err := replaceManagedTLSSymlink(s.root, "previous", currentTarget); err != nil {
		return err
	}
	return syncManagedTLSDirectory(s.root)
}

func (s *managedTLSFileStore) switchCurrent(versionName string) error {
	if runtime.GOOS == "windows" {
		return s.switchCurrentWindows(versionName)
	}
	target := filepath.Join("versions", versionName)
	currentPath := filepath.Join(s.root, "current")
	oldTarget, _ := os.Readlink(currentPath)
	if oldTarget == target {
		return nil
	}
	if err := replaceManagedTLSSymlink(s.root, "current", target); err != nil {
		return err
	}
	if oldTarget != "" && oldTarget != target {
		if err := replaceManagedTLSSymlink(s.root, "previous", oldTarget); err != nil {
			return err
		}
	}
	return syncManagedTLSDirectory(s.root)
}

func (s *managedTLSFileStore) switchCurrentWindows(versionName string) error {
	source := filepath.Join(s.root, "versions", versionName)
	temp := filepath.Join(s.root, ".current.tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := copyManagedTLSDirectory(source, temp); err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	current := filepath.Join(s.root, "current")
	previous := filepath.Join(s.root, "previous")
	if currentCert, err := readManagedTLSCertificate(current); err == nil {
		targetCert, targetErr := readManagedTLSCertificate(source)
		if targetErr == nil && sameManagedTLSCertificate(currentCert, targetCert) {
			return nil
		}
		_ = os.RemoveAll(previous)
		if err := os.Rename(current, previous); err != nil {
			return err
		}
	}
	if err := os.Rename(temp, current); err != nil {
		if _, previousErr := os.Stat(previous); previousErr == nil {
			_ = os.Rename(previous, current)
		}
		return err
	}
	return syncManagedTLSDirectory(s.root)
}

func (s *managedTLSFileStore) cleanupVersions() error {
	entries, err := os.ReadDir(filepath.Join(s.root, "versions"))
	if err != nil {
		return err
	}
	type versionEntry struct {
		name    string
		modTime time.Time
	}
	versions := make([]versionEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		versions = append(versions, versionEntry{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].modTime.After(versions[j].modTime) })
	keep := map[string]bool{}
	if runtime.GOOS != "windows" {
		for _, link := range []string{"current", "previous"} {
			if target, readErr := os.Readlink(filepath.Join(s.root, link)); readErr == nil {
				keep[filepath.Base(target)] = true
			}
		}
	}
	for index, version := range versions {
		if index < 2 || keep[version.name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.root, "versions", version.name)); err != nil {
			return err
		}
	}
	return syncManagedTLSDirectory(filepath.Join(s.root, "versions"))
}

func readManagedTLSCertificate(directory string) (*managedTLSLocalCertificate, error) {
	metadataBytes, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
	if err != nil {
		return nil, err
	}
	certificate := &managedTLSLocalCertificate{}
	if err := json.Unmarshal(metadataBytes, &certificate.Metadata); err != nil {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	certificate.FullchainPEM, err = os.ReadFile(filepath.Join(directory, "fullchain.pem"))
	if err != nil {
		return nil, err
	}
	certificate.PrivateKeyPEM, err = os.ReadFile(filepath.Join(directory, "private.key"))
	if err != nil {
		return nil, err
	}
	if err := validateManagedTLSLocalCertificate(certificate, time.Now(), false); err != nil {
		return nil, err
	}
	return certificate, nil
}

func validateManagedTLSLocalCertificate(certificate *managedTLSLocalCertificate, now time.Time, checkTime bool) error {
	if certificate == nil || certificate.Metadata.ScopeID == 0 || certificate.Metadata.Version == 0 ||
		!validManagedTLSDomain(certificate.Metadata.Domain) || len(certificate.FullchainPEM) == 0 || len(certificate.PrivateKeyPEM) == 0 {
		return errManagedTLSLocalCertificateInvalid
	}
	certBlock, _ := pem.Decode(certificate.FullchainPEM)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		return errManagedTLSLocalCertificateInvalid
	}
	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil || len(leaf.DNSNames) != 1 || !sameManagedTLSDomain(leaf.DNSNames[0], certificate.Metadata.Domain) {
		return errManagedTLSLocalCertificateInvalid
	}
	if err := leaf.VerifyHostname(certificate.Metadata.Domain); err != nil {
		return errManagedTLSLocalCertificateInvalid
	}
	if len(leaf.ExtKeyUsage) > 0 {
		serverUsage := false
		for _, usage := range leaf.ExtKeyUsage {
			if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
				serverUsage = true
				break
			}
		}
		if !serverUsage {
			return errManagedTLSLocalCertificateInvalid
		}
	}
	if checkTime && (now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter)) {
		return errManagedTLSLocalCertificateInvalid
	}
	if certificate.Metadata.NotAfter != leaf.NotAfter.Unix() {
		return errManagedTLSLocalCertificateInvalid
	}
	fingerprint := sha256.Sum256(leaf.Raw)
	if !strings.EqualFold(hex.EncodeToString(fingerprint[:]), certificate.Metadata.CertificateSHA256) {
		return errManagedTLSLocalCertificateInvalid
	}
	privatePublic, err := managedTLSPrivatePublicKey(certificate.PrivateKeyPEM)
	if err != nil {
		return errManagedTLSLocalCertificateInvalid
	}
	leafPublicDER, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return errManagedTLSLocalCertificateInvalid
	}
	privatePublicDER, err := x509.MarshalPKIXPublicKey(privatePublic)
	if err != nil || !bytes.Equal(leafPublicDER, privatePublicDER) {
		return errManagedTLSLocalCertificateInvalid
	}
	certificate.NotBefore = leaf.NotBefore
	certificate.NotAfter = leaf.NotAfter
	return nil
}

func managedTLSPrivatePublicKey(data []byte) (crypto.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return &key.PublicKey, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return &key.PublicKey, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errManagedTLSLocalCertificateInvalid
	}
	switch value := key.(type) {
	case *rsa.PrivateKey:
		return &value.PublicKey, nil
	case *ecdsa.PrivateKey:
		return &value.PublicKey, nil
	case crypto.Signer:
		return value.Public(), nil
	default:
		return nil, errManagedTLSLocalCertificateInvalid
	}
}

func writeManagedTLSFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func replaceManagedTLSSymlink(root, name, target string) error {
	temp := filepath.Join(root, "."+name+".tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.Symlink(target, temp); err != nil {
		return err
	}
	defer os.Remove(temp)
	if err := os.Rename(temp, filepath.Join(root, name)); err != nil {
		return err
	}
	return nil
}

func syncManagedTLSDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func copyManagedTLSDirectory(source, target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"metadata.json", "fullchain.pem", "private.key"} {
		sourceFile, err := os.Open(filepath.Join(source, name))
		if err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if name == "fullchain.pem" {
			mode = 0o644
		}
		targetFile, err := os.OpenFile(filepath.Join(target, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			sourceFile.Close()
			return err
		}
		_, copyErr := io.Copy(targetFile, sourceFile)
		sourceFile.Close()
		if copyErr == nil {
			copyErr = targetFile.Sync()
		}
		closeErr := targetFile.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return syncManagedTLSDirectory(target)
}

func sameManagedTLSCertificate(left, right *managedTLSLocalCertificate) bool {
	return left != nil && right != nil && left.Metadata == right.Metadata &&
		bytes.Equal(left.FullchainPEM, right.FullchainPEM) && bytes.Equal(left.PrivateKeyPEM, right.PrivateKeyPEM)
}

func sameManagedTLSDomain(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(left), "."), strings.TrimSuffix(strings.TrimSpace(right), "."))
}

func validManagedTLSDomain(domain string) bool {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return domain != "" && strings.Contains(domain, ".") && !strings.ContainsAny(domain, "*/\\: \t\r\n")
}
