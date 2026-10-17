package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wyx2685/v2node/common/instance"
	selfversion "github.com/wyx2685/v2node/common/version"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

type controlScope struct {
	Host      string `json:"host"`
	Key       string `json:"key"`
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	MachineIP string `json:"machine_ip"`
}

type cleanupTask struct {
	TaskID     string `json:"task_id"`
	Action     string `json:"action"`
	ControlID  string `json:"control_id"`
	NodeID     int    `json:"node_id"`
	ConfigHash string `json:"config_hash"`
	ExpiresAt  int64  `json:"expires_at"`
}

// Credentials exist only in this local 0600 journal while delivery is unacknowledged.
// It survives removing the last Nodes entry and a process crash before reporting.
type cleanupJournal struct {
	Scope     controlScope `json:"scope"`
	Task      cleanupTask  `json:"task"`
	AfterHash string       `json:"after_hash"`
	Status    string       `json:"status"`
	Code      string       `json:"code"`
}

type CleanupRequest struct {
	AppliedDigest string
	journal       cleanupJournal
	done          chan struct{}
}

type CleanupManager struct {
	path     string
	client   *http.Client
	Requests chan *CleanupRequest
}

func NewCleanupManager(path string) (*CleanupManager, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &CleanupManager{
		path: filepath.Clean(absolute), Requests: make(chan *CleanupRequest),
		client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (m *CleanupManager) scope(cfg conf.NodeConfig) controlScope {
	host := normalizeAPIHost(cfg.APIHost)
	machine := instance.ResolveMachineID(host)
	return controlScope{Host: host, Key: cfg.Key, ID: "cfg-" + controlDigest([]byte(machine+"|"+m.path+"|"+cfg.Key)), MachineID: machine, MachineIP: cfg.MachineIP}
}

func (m *CleanupManager) journalPath() string { return m.path + ".orphan-control.json" }

func (m *CleanupManager) save(j cleanupJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return controlWriteAtomic(m.journalPath(), data, false)
}

func (m *CleanupManager) call(ctx context.Context, scope controlScope, action string, payload any, result any) error {
	u, err := url.Parse(scope.Host)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid panel address")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(scope.Host, "/")+"/api/v2/server/control/"+action, bytes.NewReader(data))
	if err != nil {
		return errors.New("control request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+scope.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return errors.New("control request unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("control request rejected")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&envelope) != nil || len(envelope.Data) == 0 {
		return errors.New("invalid control response")
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return errors.New("invalid control response")
	}
	return nil
}

func (m *CleanupManager) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	// Retain recently used scopes in memory so zero-entry inventories are reported too.
	known := map[string]controlScope{}
	seen := map[string]time.Time{}
	lastLog := time.Time{}
	for {
		if err := m.cycle(ctx, known, seen); err != nil && time.Since(lastLog) > 5*time.Minute {
			log.Warn("SNTP 残留节点管理通道暂不可用，将自动重试")
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *CleanupManager) deliverJournal(ctx context.Context, known map[string]controlScope, seen map[string]time.Time) error {
	if data, err := os.ReadFile(m.journalPath()); err == nil {
		var j cleanupJournal
		if json.Unmarshal(data, &j) != nil || !m.validTask(j.Scope, j.Task) {
			return errors.New("invalid cleanup journal")
		}
		if j.Status != "" && time.Now().Unix() > j.Task.ExpiresAt+30*86400 {
			log.Warn("SNTP 清理回执超过面板保留期，停止重试并移除本地临时凭据")
			return os.Remove(m.journalPath())
		}
		known[j.Scope.ID], seen[j.Scope.ID] = j.Scope, time.Now()
		if j.Status == "" {
			if err := m.dispatch(ctx, j); err != nil {
				return err
			}
			data, err = os.ReadFile(m.journalPath())
			if os.IsNotExist(err) {
				return nil
			} // execution already delivered its receipt
			if err != nil || json.Unmarshal(data, &j) != nil || j.Status == "" {
				return errors.New("cleanup awaiting execution")
			}
		}
		if err := m.sendReceipt(ctx, j); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	return nil
}

func (m *CleanupManager) sendReceipt(ctx context.Context, j cleanupJournal) error {
	var result struct {
		Accepted bool `json:"accepted"`
	}
	if err := m.call(ctx, j.Scope, "report", map[string]any{
		"control_id": j.Scope.ID, "task_id": j.Task.TaskID, "status": j.Status, "result_code": j.Code,
	}, &result); err != nil {
		return err
	}
	if !result.Accepted {
		return errors.New("receipt not acknowledged")
	}
	if err := os.Remove(m.journalPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *CleanupManager) cycle(ctx context.Context, known map[string]controlScope, seen map[string]time.Time) error {
	journalErr := m.deliverJournal(ctx, known, seen)
	_, pendingErr := os.Stat(m.journalPath())
	canAccept := os.IsNotExist(pendingErr) && journalErr == nil

	before, err := os.ReadFile(m.path)
	if err != nil {
		return err
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(m.path); err != nil {
		return errors.New("invalid configuration")
	}
	after, err := os.ReadFile(m.path)
	if err != nil || !bytes.Equal(before, after) {
		return errors.New("configuration changed")
	}
	ids := map[string][]int{}
	for _, entry := range cfg.NodeConfigs {
		scope := m.scope(entry)
		known[scope.ID], seen[scope.ID] = scope, time.Now()
		found := false
		for _, id := range ids[scope.ID] {
			if id == entry.NodeID {
				found = true
			}
		}
		if !found {
			ids[scope.ID] = append(ids[scope.ID], entry.NodeID)
		}
	}
	lastErr := journalErr
	for id, scope := range known {
		if time.Since(seen[id]) > 24*time.Hour {
			delete(known, id)
			delete(seen, id)
			continue
		}
		nodeIDs := ids[id]
		if nodeIDs == nil {
			nodeIDs = []int{}
		}
		var result struct {
			Task *cleanupTask `json:"task"`
		}
		err := m.call(ctx, scope, "heartbeat", map[string]any{
			"control_id": id, "machine_id": scope.MachineID, "machine_ip": scope.MachineIP,
			"capability": "orphan_cleanup_v1", "current_version": selfversion.Current(), "accept_tasks": canAccept,
			"config_hash": controlDigest(before), "node_ids": nodeIDs,
		}, &result)
		if err != nil {
			lastErr = err
			continue
		}
		if result.Task != nil && canAccept {
			if !m.validTask(scope, *result.Task) {
				lastErr = errors.New("invalid cleanup task")
				continue
			}
			j := cleanupJournal{Scope: scope, Task: *result.Task}
			if err := m.save(j); err != nil {
				j.Status, j.Code = "failed", "state_failed"
				_ = m.sendReceipt(ctx, j)
				return err
			}
			// One journal at a time, including across panels. Finish its receipt before any next task.
			return m.dispatch(ctx, j)
		}
	}
	return lastErr
}

var cleanupTaskIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var cleanupHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (m *CleanupManager) validTask(scope controlScope, task cleanupTask) bool {
	expected := m.scope(conf.NodeConfig{APIHost: scope.Host, Key: scope.Key})
	return task.Action == "remove_orphan" && task.ControlID == scope.ID && expected.ID == scope.ID &&
		task.NodeID > 0 && task.NodeID <= 2147483647 && cleanupTaskIDPattern.MatchString(task.TaskID) && cleanupHashPattern.MatchString(task.ConfigHash)
}

func (m *CleanupManager) dispatch(ctx context.Context, j cleanupJournal) error {
	req := &CleanupRequest{journal: j, done: make(chan struct{})}
	select {
	case m.Requests <- req:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-req.done:
		if req.journal.Status != "" {
			return m.sendReceipt(ctx, req.journal)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Execute is called by cmd's single runtime owner, including in management-only startup.
// The shared updater locks also exclude binary/API-host/access-audit update tasks.
func (m *CleanupManager) Execute(ctx context.Context, req *CleanupRequest, nodes *Node, server *core.V2Core) bool {
	defer close(req.done)
	if !updateRunMu.TryLock() {
		return false
	}
	defer updateRunMu.Unlock()
	release, acquired, err := acquireUpdateLock()
	if err != nil || !acquired {
		return false
	}
	defer release()
	j := req.journal
	finish := func(status, code string) bool {
		j.Status, j.Code = status, code
		req.journal = j
		if m.save(j) != nil {
			log.Warn("SNTP 清理结果本地持久化失败，将重新核对")
		}
		return status == "success"
	}
	if !m.validTask(j.Scope, j.Task) {
		return false
	}
	if strings.ToLower(filepath.Ext(m.path)) != ".json" {
		return finish("failed", "unsupported_config")
	}
	before, err := os.ReadFile(m.path)
	if err != nil {
		return finish("failed", "invalid_config")
	}
	currentHash := controlDigest(before)
	// A prepared journal with the exact resulting hash proves the disk mutation completed
	// before a crash. Never remove a newly re-added entry while recovering this task.
	recoverApplied := j.AfterHash != "" && currentHash == j.AfterHash
	if !recoverApplied {
		if time.Now().Unix() >= j.Task.ExpiresAt {
			return finish("skipped", "expired_or_finished")
		}
		if currentHash != j.Task.ConfigHash {
			return finish("failed", "config_changed")
		}
		var verification struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason"`
		}
		if m.call(ctx, j.Scope, "verify", map[string]any{"control_id": j.Scope.ID, "task_id": j.Task.TaskID, "config_hash": currentHash}, &verification) != nil {
			return false // retry without any destructive action during network/auth failures
		}
		if !verification.Allowed {
			code := verification.Reason
			if code != "node_restored" && code != "config_changed" && code != "expired_or_finished" {
				code = "verification_failed"
			}
			return finish("skipped", code)
		}
		if time.Now().Unix() >= j.Task.ExpiresAt {
			return finish("skipped", "expired_or_finished")
		}
		after, removed, err := orphanCandidate(before, j.Scope, j.Task.NodeID)
		if err != nil {
			return finish("failed", err.Error())
		}
		j.AfterHash = controlDigest(after)
		if err := m.save(j); err != nil {
			return finish("failed", "state_failed")
		}
		if removed {
			if err := writeOrphanConfig(m.path, before, after, j.Task.TaskID); err != nil {
				return finish("failed", err.Error())
			}
			j.Code = "removed"
		} else {
			j.Code = "already_absent"
		}
	}
	req.AppliedDigest = j.AfterHash
	if nodes != nil && server != nil {
		if err := nodes.removeOrphanRuntime(j.Scope, j.Task.NodeID, server); err != nil {
			// Keep prepared state for retry: disk completion alone is not success.
			log.Warn("SNTP 残留节点配置已写入，运行态清理待重试")
			if time.Now().Unix() >= j.Task.ExpiresAt {
				return finish("failed", "runtime_failed")
			}
			return false
		}
	}
	latest, err := os.ReadFile(m.path)
	if err != nil || controlDigest(latest) != j.AfterHash {
		return finish("failed", "config_changed")
	}
	if j.Code == "" {
		j.Code = "removed"
	}
	return finish("success", j.Code)
}

// ConfigDigest lets the owner ignore only its own successful atomic-write notification.
func (m *CleanupManager) ConfigDigest() string {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return ""
	}
	return controlDigest(data)
}
