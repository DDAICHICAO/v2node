package node

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

type NodeAPICredentialMigrationResult struct {
	NodeID int    `json:"node_id"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

type NodeAPICredentialMigrationReport struct {
	Changed   bool                               `json:"changed"`
	CheckedAt int64                              `json:"checked_at"`
	NextIndex int                                `json:"next_index"`
	Error     string                             `json:"error,omitempty"`
	Results   []NodeAPICredentialMigrationResult `json:"results"`
}

// MigrateNodeAPICredentials is best effort. Failure never clears an existing
// key, and a new key is used only after it has been verified and saved.
func MigrateNodeAPICredentials(ctx context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	report, err := migrateNodeAPICredentials(ctx, path, func(ctx context.Context, cfg conf.NodeConfig) (string, error) {
		client, err := panel.New(&cfg)
		if err != nil {
			return "", errors.New("credential_config_invalid")
		}
		return client.ExchangeNodeAPICredential(ctx)
	})
	report.CheckedAt = time.Now().Unix()
	if err != nil {
		report.Error = credentialMigrationCode(err)
		log.WithField("reason", report.Error).Warn("SNTP 节点密钥自动迁移未完成，保留现有配置后重试")
	}
	for _, result := range report.Results {
		if result.Status == "pending" {
			log.WithFields(log.Fields{"node_id": result.NodeID, "reason": result.Code}).Warn("SNTP 节点密钥暂未迁移")
		}
	}
	if report.Changed {
		log.Warn("SNTP 节点专用密钥已验证并写入配置")
	}
	// This report contains only node numbers and bounded result codes, never
	// keys, panel addresses, source IPs or remote response/error text.
	if output, marshalErr := json.MarshalIndent(report, "", "  "); marshalErr == nil {
		if writeErr := controlWriteAtomic(path+".node-api-migration.json", append(output, '\n'), false); writeErr != nil {
			log.Warn("SNTP 节点密钥迁移报告保存失败")
		}
	}
	return report.Changed
}

// Retry after a transient failure without depending on --watch. Reload requests
// go through cmd's existing runtime owner; this goroutine never changes clients.
func RunNodeAPICredentialMigration(ctx context.Context, path string, reloadCh chan<- struct{}) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if MigrateNodeAPICredentials(ctx, path) {
				select {
				case reloadCh <- struct{}{}:
				default:
				}
			}
		}
	}
}

func migrateNodeAPICredentials(ctx context.Context, path string, exchange func(context.Context, conf.NodeConfig) (string, error)) (NodeAPICredentialMigrationReport, error) {
	report := NodeAPICredentialMigrationReport{Results: []NodeAPICredentialMigrationResult{}}
	if !updateRunMu.TryLock() {
		return report, errors.New("credential_update_busy")
	}
	defer updateRunMu.Unlock()
	release, acquired, err := acquireUpdateLock()
	if err != nil || !acquired {
		return report, errors.New("credential_update_busy")
	}
	defer release()
	// Finish any prepared cleanup and its receipt before changing control IDs.
	if _, err := os.Lstat(path + ".orphan-control.json"); !os.IsNotExist(err) {
		return report, errors.New("credential_control_pending")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0200 == 0 || !strings.EqualFold(filepath.Ext(path), ".json") {
		return report, errors.New("credential_config_not_writable")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		return report, errors.New("credential_config_invalid")
	}
	var root map[string]json.RawMessage
	if !jsontext.Value(before).IsValid() || json.Unmarshal(before, &root) != nil || root == nil {
		return report, errors.New("credential_config_invalid")
	}
	for key := range root {
		if strings.EqualFold(key, "Nodes") && key != "Nodes" {
			return report, errors.New("credential_config_ambiguous")
		}
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(root["Nodes"], &entries) != nil {
		return report, errors.New("credential_config_invalid")
	}
	for _, fields := range entries {
		for key := range fields {
			for _, canonical := range []string{"ApiHost", "ApiKey", "NodeID"} {
				if strings.EqualFold(key, canonical) && key != canonical {
					return report, errors.New("credential_config_ambiguous")
				}
			}
		}
	}
	cfg := conf.New()
	if cfg.LoadFromPath(path) != nil || len(cfg.NodeConfigs) != len(entries) {
		return report, errors.New("credential_config_invalid")
	}
	latest, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, latest) {
		return report, errors.New("credential_config_changed")
	}
	start := 0
	var previous struct {
		NextIndex int `json:"next_index"`
	}
	if summary, readErr := os.ReadFile(path + ".node-api-migration.json"); readErr == nil &&
		json.Unmarshal(summary, &previous) == nil && previous.NextIndex >= 0 && previous.NextIndex < len(cfg.NodeConfigs) {
		start = previous.NextIndex
	}
	changed, resumeSet := false, false
	for offset := range cfg.NodeConfigs {
		i := (start + offset) % len(cfg.NodeConfigs)
		entry := cfg.NodeConfigs[i]
		result := NodeAPICredentialMigrationResult{NodeID: entry.NodeID, Status: "pending"}
		switch {
		case panel.IsNodeAPICredentialFor(entry.Key, entry.NodeID):
			result.Status = "already_scoped"
		case strings.HasPrefix(entry.Key, "sntp-node-"):
			result.Code = "credential_scope_invalid"
		default:
			endpoint, parseErr := url.Parse(entry.APIHost)
			if parseErr != nil || endpoint.Scheme != "https" {
				result.Code = "credential_https_required"
			} else if ctx.Err() != nil {
				result.Code = "credential_timeout"
				if !resumeSet {
					report.NextIndex, resumeSet = i, true
				}
			} else if key, exchangeErr := exchange(ctx, entry); exchangeErr != nil {
				result.Code = credentialMigrationCode(exchangeErr)
			} else if !panel.IsNodeAPICredentialFor(key, entry.NodeID) {
				result.Code = "credential_scope_invalid"
			} else {
				entries[i]["ApiKey"], _ = json.Marshal(key)
				result.Status = "verified"
				changed = true
			}
		}
		report.Results = append(report.Results, result)
	}
	if !changed {
		return report, nil
	}
	root["Nodes"], _ = json.Marshal(entries)
	after, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return report, errors.New("credential_config_invalid")
	}
	if err := writeNodeCredentialConfig(path, before, append(after, '\n')); err != nil {
		for i := range report.Results {
			if report.Results[i].Status == "verified" {
				report.Results[i].Status = "pending"
				report.Results[i].Code = credentialMigrationCode(err)
			}
		}
		return report, err
	}
	report.Changed = true
	for i := range report.Results {
		if report.Results[i].Status == "verified" {
			report.Results[i].Status = "migrated"
		}
	}
	return report, nil
}

func writeNodeCredentialConfig(path string, before, after []byte) error {
	backupPath := path + ".node-api-credential.bak"
	backup, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		info, statErr := os.Lstat(backupPath)
		if statErr != nil || !info.Mode().IsRegular() || os.Chmod(backupPath, 0600) != nil {
			return errors.New("credential_backup_failed")
		}
		existing, readErr := os.ReadFile(backupPath)
		if readErr != nil || !jsontext.Value(existing).IsValid() {
			return errors.New("credential_backup_failed")
		}
	} else if err != nil {
		return errors.New("credential_backup_failed")
	} else {
		_, writeErr := backup.Write(before)
		syncErr := backup.Sync()
		closeErr := backup.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			_ = os.Remove(backupPath)
			return errors.New("credential_backup_failed")
		}
	}
	if err := controlWriteAtomicChecked(path, after, true, before); err != nil {
		if err.Error() == "config_changed" {
			return errors.New("credential_config_changed")
		}
		return errors.New("credential_write_failed")
	}
	return nil
}

func credentialMigrationCode(err error) string {
	if err != nil {
		switch err.Error() {
		case "credential_update_busy", "credential_control_pending", "credential_config_not_writable",
			"credential_config_invalid", "credential_config_ambiguous", "credential_config_changed",
			"credential_https_required", "credential_transport_failed", "credential_request_rejected",
			"credential_response_invalid", "credential_verification_failed", "credential_backup_failed", "credential_write_failed":
			return err.Error()
		}
	}
	return "credential_exchange_failed"
}
