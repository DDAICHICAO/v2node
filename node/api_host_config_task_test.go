package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

func TestApplyApiHostConfigTaskReplacesAllNodeApiHosts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	initial := []byte(`{
  "Nodes": [
    {"ApiHost": "http://anode.sntp.uk:999", "NodeID": 40, "ApiKey": "a"},
    {"ApiHost": "http://anode.sntp.uk:999", "NodeID": 401, "ApiKey": "b"}
  ]
}`)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task := panel.UpdateTask{
		TaskID:  "api-host-test",
		Type:    apiHostTaskType,
		ApiHost: &panel.ApiHostTask{ApiHost: "https://dashboard.sntp.uk"},
	}

	if err := applyApiHostConfigTask(task, configPath); err != nil {
		t.Fatalf("apply api host task: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	nodes, ok := config["Nodes"].([]any)
	if !ok || len(nodes) != 2 {
		t.Fatalf("unexpected Nodes: %#v", config["Nodes"])
	}
	for _, node := range nodes {
		item, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("unexpected node shape: %#v", node)
		}
		if item["ApiHost"] != "https://dashboard.sntp.uk" {
			t.Fatalf("ApiHost not replaced: %#v", item["ApiHost"])
		}
	}
	backups, err := filepath.Glob(configPath + ".bak.*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %d", len(backups))
	}
}

func TestApplyApiHostConfigTaskCanMatchOldApiHost(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	initial := []byte(`{
  "Nodes": [
    {"ApiHost": "http://anode.sntp.uk:999", "NodeID": 40},
    {"ApiHost": "https://dashboard.sntp.uk", "NodeID": 401}
  ]
}`)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task := panel.UpdateTask{
		TaskID: "api-host-test",
		Type:   apiHostTaskType,
		ApiHost: &panel.ApiHostTask{
			ApiHost:      "https://cf-dashboard.sntp.uk",
			MatchApiHost: "http://anode.sntp.uk:999",
		},
	}

	if err := applyApiHostConfigTask(task, configPath); err != nil {
		t.Fatalf("apply api host task: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	nodes := config["Nodes"].([]any)
	first := nodes[0].(map[string]any)
	second := nodes[1].(map[string]any)
	if first["ApiHost"] != "https://cf-dashboard.sntp.uk" {
		t.Fatalf("first ApiHost not replaced: %#v", first["ApiHost"])
	}
	if second["ApiHost"] != "https://dashboard.sntp.uk" {
		t.Fatalf("second ApiHost should stay unchanged: %#v", second["ApiHost"])
	}
}

func TestApplyApiHostConfigTaskRejectsInvalidTarget(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"Nodes":[{"ApiHost":"http://old"}]}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task := panel.UpdateTask{
		TaskID:  "api-host-test",
		Type:    apiHostTaskType,
		ApiHost: &panel.ApiHostTask{ApiHost: "ftp://dashboard.sntp.uk"},
	}
	if err := applyApiHostConfigTask(task, configPath); err == nil {
		t.Fatal("expected invalid api host error")
	}
}

func TestNodeAPICredentialMigrationPreservesFieldsAndSeparatesNodeIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"Unknown":{"large":9007199254740993},"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old","Timeout":9,"Extra":true},{"ApiHost":"https://panel.example","NodeID":8,"ApiKey":"old"}]}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	keys := map[int]string{7: "sntp-node-v1.v2node.7." + strings.Repeat("a", 64), 8: "sntp-node-v1.v2node.8." + strings.Repeat("b", 64)}
	calls := 0
	exchange := func(ctx context.Context, cfg conf.NodeConfig) (string, error) {
		calls++
		if cfg.Key != "old" {
			t.Error("migration did not use this entry's existing key")
		}
		return keys[cfg.NodeID], nil
	}
	report, err := migrateNodeAPICredentials(context.Background(), path, exchange)
	if err != nil || !report.Changed || calls != 2 {
		t.Fatalf("migration failed: %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Contains(after, []byte("9007199254740993")) || !bytes.Contains(after, []byte(`"Extra": true`)) {
		t.Fatal("unrelated configuration was changed")
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	for _, entry := range cfg.NodeConfigs {
		if entry.Key != keys[entry.NodeID] {
			t.Fatal("node received another node's key")
		}
	}
	backup, err := os.ReadFile(path + ".node-api-credential.bak")
	if err != nil || !bytes.Equal(backup, before) {
		t.Fatal("original configuration backup missing")
	}
	report, err = migrateNodeAPICredentials(context.Background(), path, exchange)
	if err != nil || report.Changed || calls != 2 {
		t.Fatal("already migrated configuration was exchanged again")
	}
}

func TestNodeAPICredentialMigrationKeepsFailedEntriesAndRetriesThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old"},{"ApiHost":"https://panel.example","NodeID":8,"ApiKey":"old"}]}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	fail := true
	exchange := func(ctx context.Context, cfg conf.NodeConfig) (string, error) {
		if cfg.NodeID == 8 && fail {
			return "", errors.New("contains-private-test-key-and-address")
		}
		return "sntp-node-v1.v2node." + fmt.Sprint(cfg.NodeID) + "." + strings.Repeat("a", 64), nil
	}
	report, err := migrateNodeAPICredentials(context.Background(), path, exchange)
	if err != nil || !report.Changed {
		t.Fatalf("successful entries were not saved: %v", err)
	}
	encoded, _ := json.Marshal(report)
	if bytes.Contains(encoded, []byte("private-test")) {
		t.Fatal("report exposed unsafe error text")
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(path); err != nil || cfg.NodeConfigs[1].Key != "old" {
		t.Fatal("failed entry's key was modified")
	}
	fail = false
	if report, err = migrateNodeAPICredentials(context.Background(), path, exchange); err != nil || !report.Changed {
		t.Fatalf("retry failed: %v", err)
	}
	backup, _ := os.ReadFile(path + ".node-api-credential.bak")
	if !bytes.Equal(backup, before) {
		t.Fatal("retry overwrote the original backup")
	}
}

func TestNodeAPICredentialMigrationRefusesChangedOrAmbiguousConfiguration(t *testing.T) {
	for _, input := range []string{
		`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old"}],"nodes":[]}`,
		`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old","apikey":"other"}]}`,
		`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old","ApiKey":"other"}]}`,
		`Nodes: []`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		before := []byte(input)
		if err := os.WriteFile(path, before, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := migrateNodeAPICredentials(context.Background(), path, func(context.Context, conf.NodeConfig) (string, error) {
			t.Error("ambiguous config attempted exchange")
			return "", nil
		})
		after, _ := os.ReadFile(path)
		if err == nil || !bytes.Equal(before, after) {
			t.Fatal("ambiguous configuration was overwritten")
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	edited := []byte(`{"Nodes":[],"AdminEdit":true}`)
	report, err := migrateNodeAPICredentials(context.Background(), path, func(context.Context, conf.NodeConfig) (string, error) {
		if err := os.WriteFile(path, edited, 0600); err != nil {
			t.Fatal(err)
		}
		return "sntp-node-v1.v2node.7." + strings.Repeat("a", 64), nil
	})
	after, _ := os.ReadFile(path)
	if err == nil || report.Changed || !bytes.Equal(after, edited) {
		t.Fatal("concurrent administrator edit was lost")
	}
}

func TestNodeAPICredentialMigrationRejectsOtherNodeKeyAndPreservesOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old"}]}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := migrateNodeAPICredentials(context.Background(), path, func(context.Context, conf.NodeConfig) (string, error) {
		return "sntp-node-v1.v2node.8." + strings.Repeat("a", 64), nil
	})
	after, _ := os.ReadFile(path)
	if err != nil || report.Changed || !bytes.Equal(after, before) {
		t.Fatal("wrong node credential changed configuration")
	}
}

func TestNodeAPICredentialMigrationLeavesUnavailableConfigUnchanged(t *testing.T) {
	for _, scenario := range []string{"http", "exchange_failed", "backup_failed", "cleanup_pending", "cancelled", "update_busy"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			host := "https://panel.example"
			if scenario == "http" {
				host = "http://panel.example"
			}
			before := []byte(fmt.Sprintf(`{"Nodes":[{"ApiHost":%q,"NodeID":7,"ApiKey":"old"}]}`, host))
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			if scenario == "backup_failed" {
				if err := os.Mkdir(path+".node-api-credential.bak", 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cleanup_pending" {
				if err := os.WriteFile(path+".orphan-control.json", []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			if scenario == "update_busy" {
				updateRunMu.Lock()
				defer updateRunMu.Unlock()
			}
			report, _ := migrateNodeAPICredentials(ctx, path, func(context.Context, conf.NodeConfig) (string, error) {
				if scenario == "http" || scenario == "cleanup_pending" || scenario == "cancelled" || scenario == "update_busy" {
					t.Error("unavailable configuration attempted exchange")
				}
				if scenario == "exchange_failed" {
					return "", errors.New("credential_transport_failed")
				}
				return "sntp-node-v1.v2node.7." + strings.Repeat("a", 64), nil
			})
			after, _ := os.ReadFile(path)
			if report.Changed || !bytes.Equal(before, after) {
				t.Fatal("failed migration modified existing configuration")
			}
		})
	}
}

func TestNodeAPICredentialMigrationWritesOnlySafeSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"Nodes":[{"ApiHost":"http://private-panel.example","NodeID":7,"ApiKey":"private-key"}]}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if MigrateNodeAPICredentials(context.Background(), path) {
		t.Fatal("HTTP configuration was migrated")
	}
	data, err := os.ReadFile(path + ".node-api-migration.json")
	if err != nil || bytes.Contains(data, []byte("private-key")) || bytes.Contains(data, []byte("private-panel")) || !bytes.Contains(data, []byte("credential_https_required")) {
		t.Fatal("unsafe or missing migration summary")
	}
}

func TestNodeAPICredentialMigrationRetryStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	path := filepath.Join(t.TempDir(), "config.json")
	go func() {
		RunNodeAPICredentialMigration(ctx, path, make(chan struct{}, 1))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("migration retry ignored shutdown")
	}
}

func TestCleanupRetiresRejectedScopesAfterCredentialMigration(t *testing.T) {
	var rejectActive atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rejectActive.Load() || r.Header.Get("Authorization") == "Bearer old" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"task":null}}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config.json")
	data := []byte(fmt.Sprintf(`{"Nodes":[{"ApiHost":%q,"NodeID":7,"ApiKey":"sntp-node-v1.v2node.7.%s"}]}`, server.URL, strings.Repeat("a", 64)))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewCleanupManager(path)
	if err != nil {
		t.Fatal(err)
	}
	old := manager.scope(conf.NodeConfig{APIHost: server.URL, Key: "old"})
	known := map[string]controlScope{old.ID: old}
	seen := map[string]time.Time{old.ID: time.Now()}
	if err := manager.cycle(context.Background(), known, seen); err != nil {
		t.Fatal(err)
	}
	if _, exists := known[old.ID]; exists {
		t.Fatal("retired legacy key kept generating authorization failures")
	}
	rejectActive.Store(true)
	if err := manager.cycle(context.Background(), known, seen); err == nil || len(known) != 1 {
		t.Fatal("active scope authorization failure was suppressed")
	}
}

func TestNodeAPICredentialMigrationContinuesAfterTimedOutEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"Nodes":[{"ApiHost":"https://panel.example","NodeID":7,"ApiKey":"old"},{"ApiHost":"https://panel.example","NodeID":8,"ApiKey":"old"},{"ApiHost":"https://panel.example","NodeID":9,"ApiKey":"old"}]}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report, err := migrateNodeAPICredentials(ctx, path, func(context.Context, conf.NodeConfig) (string, error) {
		cancel()
		return "", errors.New("credential_transport_failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, _ := json.Marshal(report)
	if err := os.WriteFile(path+".node-api-migration.json", summary, 0600); err != nil {
		t.Fatal(err)
	}
	var order []int
	_, err = migrateNodeAPICredentials(context.Background(), path, func(_ context.Context, cfg conf.NodeConfig) (string, error) {
		order = append(order, cfg.NodeID)
		return "sntp-node-v1.v2node." + fmt.Sprint(cfg.NodeID) + "." + strings.Repeat("a", 64), nil
	})
	if err != nil || len(order) != 3 || order[0] != 8 {
		t.Fatal("retry was blocked again by the first timed out entry")
	}
}

func TestNodeAPICredentialMigrationHandlesHundredsOfEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	entries := make([]map[string]any, 300)
	for i := range entries {
		entries[i] = map[string]any{"ApiHost": "https://panel.example", "NodeID": i + 1, "ApiKey": "old"}
	}
	before, _ := json.Marshal(map[string]any{"Nodes": entries})
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := migrateNodeAPICredentials(context.Background(), path, func(_ context.Context, cfg conf.NodeConfig) (string, error) {
		return fmt.Sprintf("sntp-node-v1.v2node.%d.%064x", cfg.NodeID, cfg.NodeID), nil
	})
	if err != nil || !report.Changed || len(report.Results) != 300 {
		t.Fatalf("batch migration failed: %v", err)
	}
	cfg := conf.New()
	if err := cfg.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range cfg.NodeConfigs {
		if !panel.IsNodeAPICredentialFor(entry.Key, entry.NodeID) || seen[entry.Key] {
			t.Fatal("batch migration reused a credential across node IDs")
		}
		seen[entry.Key] = true
	}
}
