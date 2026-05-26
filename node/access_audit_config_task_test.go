package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestApplyAccessAuditConfigTaskMergesConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	initial := []byte(`{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  },
  "Nodes": [
    {
      "ApiHost": "http://anode.sntp.uk:999",
      "NodeID": 121,
      "ApiKey": "secret",
      "Timeout": 15
    }
  ]
}`)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	sntpAccess := true
	task := panel.UpdateTask{
		TaskID: "access-audit-test",
		Type:   accessAuditTaskType,
		AccessAudit: &panel.AccessAuditTask{
			Enabled:       true,
			Endpoint:      "https://logs.sntp.uk/api/v1/access-events",
			Token:         "token",
			BatchSize:     1000,
			MaxQueueSize:  10000,
			FlushInterval: "1s",
			Timeout:       "5s",
			SNTPAccess:    &sntpAccess,
		},
	}

	if err := applyAccessAuditConfigTask(task, configPath); err != nil {
		t.Fatalf("apply config task: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode updated config: %v", err)
	}
	logConfig, ok := config["Log"].(map[string]any)
	if !ok {
		t.Fatalf("missing Log config: %#v", config["Log"])
	}
	if logConfig["SNTPAccess"] != true {
		t.Fatalf("SNTPAccess not enabled: %#v", logConfig["SNTPAccess"])
	}
	audit, ok := config["AccessAudit"].(map[string]any)
	if !ok {
		t.Fatalf("missing AccessAudit config: %#v", config["AccessAudit"])
	}
	if audit["Enabled"] != true || audit["Endpoint"] != "https://logs.sntp.uk/api/v1/access-events" || audit["Token"] != "token" {
		t.Fatalf("unexpected AccessAudit: %#v", audit)
	}
	nodes, ok := config["Nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("Nodes not preserved: %#v", config["Nodes"])
	}
	backups, err := filepath.Glob(configPath + ".bak.*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("expected one backup, got %d", len(backups))
	}
}

func TestApplyAccessAuditConfigTaskRequiresEndpointWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"Nodes":[]}`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task := panel.UpdateTask{
		TaskID:      "access-audit-test",
		Type:        accessAuditTaskType,
		AccessAudit: &panel.AccessAuditTask{Enabled: true, Token: "token"},
	}
	if err := applyAccessAuditConfigTask(task, configPath); err == nil {
		t.Fatal("expected missing endpoint error")
	}
}
