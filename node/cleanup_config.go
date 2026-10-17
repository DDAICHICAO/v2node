package node

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/wyx2685/v2node/conf"
)

func controlDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RawMessage preserves unknown fields and numbers larger than float64's integer range.
// Only canonical JSON configurations are writable; Viper's other formats remain readable.
func orphanCandidate(data []byte, scope controlScope, nodeID int) ([]byte, bool, error) {
	var root map[string]json.RawMessage
	if !jsontext.Value(data).IsValid() || json.Unmarshal(data, &root) != nil || root == nil {
		return nil, false, errors.New("invalid_config")
	}
	for key := range root {
		if strings.EqualFold(key, "Nodes") && key != "Nodes" {
			return nil, false, errors.New("unsupported_config")
		}
	}
	var entries []json.RawMessage
	if json.Unmarshal(root["Nodes"], &entries) != nil {
		return nil, false, errors.New("invalid_config")
	}
	kept := make([]json.RawMessage, 0, len(entries))
	removed := false
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil {
			return nil, false, errors.New("invalid_config")
		}
		// Reject ambiguous aliases rather than editing a different effective Viper value.
		for key := range fields {
			for _, canonical := range []string{"ApiHost", "ApiKey", "NodeID"} {
				if strings.EqualFold(key, canonical) && key != canonical {
					return nil, false, errors.New("unsupported_config")
				}
			}
		}
		var host, key string
		var id int
		if json.Unmarshal(fields["ApiHost"], &host) != nil || json.Unmarshal(fields["ApiKey"], &key) != nil || json.Unmarshal(fields["NodeID"], &id) != nil {
			return nil, false, errors.New("invalid_config")
		}
		if normalizeAPIHost(host) == scope.Host && key == scope.Key && id == nodeID {
			removed = true
		} else {
			kept = append(kept, entry)
		}
	}
	if !removed {
		return data, false, nil
	}
	root["Nodes"], _ = json.Marshal(kept)
	output, err := json.MarshalIndent(root, "", "  ")
	return append(output, '\n'), true, err
}

func controlWriteAtomic(path string, data []byte, validate bool) error {
	return controlWriteAtomicChecked(path, data, validate, nil)
}

func controlWriteAtomicChecked(path string, data []byte, validate bool, expected []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".orphan-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if validate {
		candidate := conf.New()
		if err = candidate.LoadFromPath(tmp); err != nil {
			return errors.New("invalid_config")
		}
	}
	if expected != nil {
		latest, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(expected, latest) {
			return errors.New("config_changed")
		}
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// On filesystems supporting directory sync, persist the rename as well as file content.
	if dir, openErr := os.Open(filepath.Dir(path)); openErr == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func writeOrphanConfig(path string, before, after []byte, taskID string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0200 == 0 {
		return errors.New("write_failed")
	}
	backup := path + ".orphan-" + taskID + ".bak"
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		existing, readErr := os.ReadFile(backup)
		if readErr != nil || !bytes.Equal(existing, before) {
			return errors.New("backup_failed")
		}
	} else if err != nil {
		return errors.New("backup_failed")
	} else {
		_, err = f.Write(before)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return errors.New("backup_failed")
		}
	}
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, latest) {
		return errors.New("config_changed")
	}
	if err = controlWriteAtomicChecked(path, after, true, before); err != nil {
		if err.Error() == "config_changed" {
			return err
		}
		return errors.New("write_failed")
	}
	return nil
}
