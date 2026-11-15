package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/instance"
	selfversion "github.com/wyx2685/v2node/common/version"
	"github.com/wyx2685/v2node/conf"
)

const LegacyTransitionVersion = "v5.0.4"
const legacyTransitionPrefix = "v2node-transition:v5.0.4:"
const legacyTransitionReady = "transition runtime ready"

var legacyTransitionID = regexp.MustCompile(`^v2node-transition:v5\.0\.4:[a-f0-9]{32}$`)

// LegacyUpgradeTransition protects the first restart performed by an old updater.
// Its original executable is the exact task-specific backup created by that updater.
// The journal contains a private configuration snapshot, never an API response.
type LegacyUpgradeTransition struct {
	TaskID       string `json:"task_id"`
	Version      string `json:"version"`
	ConfigPath   string `json:"config_path"`
	ProgramPath  string `json:"program_path"`
	BackupPath   string `json:"backup_path"`
	Before       []byte `json:"config_before"`
	ExpectedHash string `json:"expected_config_sha256"`
	Status       string `json:"status"`
	statePath    string
	journalPath  string
	done         bool
}

func PrepareLegacyUpgrade(ctx context.Context, configPath, version string) (*LegacyUpgradeTransition, error) {
	binaryPath, err := os.Executable()
	if err != nil {
		return nil, errors.New("transition_program_unavailable")
	}
	return prepareLegacyTransition(ctx, configPath, binaryPath, updateStatePath(), version, PreflightNodeAPIUpgrade)
}

func configHash(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func regularTransitionFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func transitionWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".v2node-transition-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (t *LegacyUpgradeTransition) saveJournal() error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return transitionWrite(t.journalPath, data, 0600)
}

func (t *LegacyUpgradeTransition) saveState(status, message string) error {
	data, err := json.Marshal(updateState{TaskID: t.TaskID, Version: t.Version, Status: status, Message: message, UpdatedAt: time.Now().Unix()})
	if err != nil {
		return err
	}
	return transitionWrite(t.statePath, data, 0600)
}

func prepareLegacyTransition(ctx context.Context, configPath, binaryPath, statePath, version string, preflight func(context.Context, string) (NodeAPICredentialMigrationReport, error)) (*LegacyUpgradeTransition, error) {
	if version != LegacyTransitionVersion {
		return nil, nil
	}
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, errors.New("transition_config_unavailable")
	}
	var state updateState
	if data, err := os.ReadFile(statePath); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	// The old updater logs state-write errors but still restarts. Recover its
	// narrowly named backup even in that case; never silently skip the guard.
	if !legacyTransitionID.MatchString(state.TaskID) || state.Version != version {
		paths, _ := filepath.Glob(binaryPath + ".bak." + safeFilePart(legacyTransitionPrefix) + "*")
		var latest time.Time
		state = updateState{}
		for _, path := range paths {
			suffix := strings.TrimPrefix(path, binaryPath+".bak."+safeFilePart(legacyTransitionPrefix))
			id := legacyTransitionPrefix + suffix
			if !legacyTransitionID.MatchString(id) || !regularTransitionFile(path) {
				continue
			}
			var journal LegacyUpgradeTransition
			if data, err := os.ReadFile(path + ".transition.json"); err == nil && json.Unmarshal(data, &journal) == nil && (journal.Status == "committed" || journal.Status == "rolled_back") {
				continue
			}
			info, _ := os.Stat(path)
			if info != nil && (state.TaskID == "" || info.ModTime().After(latest)) {
				state = updateState{TaskID: id, Version: version, Status: "success"}
				latest = info.ModTime()
			}
		}
	}
	if state.TaskID == "" || !legacyTransitionID.MatchString(state.TaskID) || state.Version != version {
		return nil, nil
	}
	if state.Status == "failed" || state.Status == "skipped" {
		return nil, nil
	}
	t := &LegacyUpgradeTransition{TaskID: state.TaskID, Version: version, ConfigPath: configPath, ProgramPath: binaryPath, BackupPath: binaryPath + ".bak." + safeFilePart(state.TaskID), statePath: statePath, Status: "preparing"}
	t.journalPath = t.BackupPath + ".transition.json"
	if !regularTransitionFile(t.BackupPath) {
		return nil, errors.New("transition_backup_unavailable")
	}
	if data, err := os.ReadFile(t.journalPath); err == nil {
		var saved LegacyUpgradeTransition
		if json.Unmarshal(data, &saved) != nil || saved.TaskID != t.TaskID || saved.ConfigPath != configPath || saved.ProgramPath != binaryPath || saved.BackupPath != t.BackupPath {
			return nil, errors.New("transition_journal_invalid")
		}
		if saved.Status == "committed" || saved.Status == "rolled_back" {
			return nil, nil
		}
		t.Before, t.ExpectedHash = saved.Before, saved.ExpectedHash
		if current, err := os.ReadFile(configPath); err != nil || configHash(current) != t.ExpectedHash {
			return t, errors.New("transition_config_changed")
		}
	} else if !os.IsNotExist(err) {
		return nil, errors.New("transition_journal_unavailable")
	}
	if t.Before == nil {
		t.Before, err = os.ReadFile(configPath)
		if err != nil {
			return t, errors.New("transition_config_unavailable")
		}
		t.ExpectedHash = configHash(t.Before)
	}
	if err := t.saveJournal(); err != nil {
		return t, errors.New("transition_journal_failed")
	}
	if err := t.saveState("installing", "verifying all configured node credentials"); err != nil {
		return t, errors.New("transition_state_failed")
	}
	if _, err := preflight(ctx, configPath); err != nil {
		return t, errors.New(credentialMigrationCode(err))
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		return t, errors.New("transition_config_unavailable")
	}
	t.ExpectedHash = configHash(after)
	if err := t.saveJournal(); err != nil {
		return t, errors.New("transition_journal_failed")
	}
	return t, nil
}

func (t *LegacyUpgradeTransition) BinaryPath() string { return t.ProgramPath }

func (t *LegacyUpgradeTransition) Commit() error {
	if t == nil || t.done {
		return nil
	}
	if err := t.saveState("success", legacyTransitionReady); err != nil {
		return err
	}
	t.Status = "committed"
	if err := t.saveJournal(); err != nil {
		t.Status = "preparing"
		return err
	}
	t.done = true
	return nil
}

// Rollback preserves external edits. A config written by this transition alone
// may be restored; an operator's newer bytes must never be overwritten.
func (t *LegacyUpgradeTransition) Rollback(code string) error {
	if t == nil || t.done {
		return nil
	}
	program, err := os.ReadFile(t.BackupPath)
	if err != nil || !regularTransitionFile(t.BackupPath) {
		return errors.New("transition_backup_unavailable")
	}
	if current, err := os.ReadFile(t.ConfigPath); err == nil && t.Before != nil && configHash(current) == t.ExpectedHash && !bytes.Equal(current, t.Before) {
		if err := transitionWrite(t.ConfigPath, t.Before, 0600); err != nil {
			return errors.New("transition_config_restore_failed")
		}
	}
	if err := transitionWrite(t.ProgramPath, program, 0755); err != nil {
		return errors.New("transition_program_restore_failed")
	}
	t.Status, t.done = "rolled_back", true
	// The restored program can run even if the old state directory is unwritable.
	_ = t.saveState("failed", "transition rolled back: "+code)
	_ = t.saveJournal()
	return nil
}

// ReportRollback only reports within each configured identity. It deliberately
// advertises no new capabilities after the original executable was restored.
func (t *LegacyUpgradeTransition) ReportRollback(ctx context.Context) {
	cfg := conf.New()
	if cfg.LoadFromPath(t.ConfigPath) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	previous := selfversion.FromCommand(t.ProgramPath)
	for _, entry := range cfg.NodeConfigs {
		endpoint, err := url.Parse(entry.APIHost)
		if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			continue
		}
		endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/api/v2/server/update/report"
		query := endpoint.Query()
		query.Set("node_id", strconv.Itoa(entry.NodeID))
		query.Set("node_type", "v2node")
		query.Set("instance_id", instance.ResolveID(entry.APIHost, entry.NodeID))
		query.Set("current_version", previous)
		endpoint.RawQuery = query.Encode()
		data, _ := json.Marshal(panel.UpdateReport{TaskID: t.TaskID, Version: t.Version, CurrentVersion: previous, Status: "failed", Message: "过渡升级未通过检查，已回退原程序，请查看本机日志"})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+entry.Key)
		req.Header.Set("Content-Type", "application/json")
		if response, err := client.Do(req); err == nil {
			response.Body.Close()
		}
	}
}
