package instance

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const persistedIDFileName = "instance_id"

// ResolveID returns a stable, non-secret identifier for this v2node process host.
// It is namespaced by panel host and node id so one physical machine can dock
// multiple panel nodes without sharing the same instance key.
func ResolveID(apiHost string, nodeID int) string {
	hostname := readHostname()
	seed := readOrCreatePersistedID()
	if seed == "" {
		seed = readMachineID()
	}
	if seed == "" {
		seed = hostname
	}
	if seed == "" {
		seed = "unknown"
	}

	sum := sha256.Sum256([]byte(apiHost + "|" + strconv.Itoa(nodeID) + "|" + seed))
	prefix := sanitize(hostname)
	if prefix == "" {
		prefix = "node"
	}
	if len(prefix) > 32 {
		prefix = prefix[:32]
	}
	return prefix + "-" + hex.EncodeToString(sum[:])[:12]
}

func readHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(hostname)
}

func readMachineID() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		bytes, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		id := strings.TrimSpace(string(bytes))
		if id != "" {
			return id
		}
	}
	return ""
}

func readOrCreatePersistedID() string {
	for _, dir := range candidateDirs() {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			continue
		}
		path := filepath.Join(dir, persistedIDFileName)
		if bytes, err := os.ReadFile(path); err == nil {
			if id := strings.TrimSpace(string(bytes)); id != "" {
				return id
			}
		}

		id := randomHex(16)
		if id == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(id+"\n"), 0600); err == nil {
			return id
		}
	}
	return ""
}

func candidateDirs() []string {
	dirs := []string{"/etc/v2node"}
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		dirs = append(dirs, filepath.Join(configDir, "v2node"))
	}
	if runtime.GOOS == "windows" {
		dirs = dirs[1:]
	}
	return dirs
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

func sanitize(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '.' || r == ':' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_.:-")
}
