package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
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
