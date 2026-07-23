package node

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
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
			SpoolPath:     "/srv/v2node/access.db",
			MaxSpoolBytes: 536870912,
			MaxSpoolAge:   "72h",
			SNTPAccess:    &sntpAccess,
			FlowTraffic: &panel.FlowTrafficTask{
				Enabled:            true,
				CheckpointInterval: "5m",
				SpoolPath:          "/var/lib/v2node/access-audit-spool/flow.db",
				MaxSpoolBytes:      268435456,
				MaxSpoolAge:        "24h",
			},
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
	if audit["SpoolPath"] != "/srv/v2node/access.db" ||
		audit["MaxSpoolBytes"] != float64(536870912) ||
		audit["MaxSpoolAge"] != "72h" {
		t.Fatalf("unexpected access spool config: %#v", audit)
	}
	flowTraffic, ok := audit["FlowTraffic"].(map[string]any)
	if !ok || flowTraffic["Enabled"] != true || flowTraffic["CheckpointInterval"] != "5m" || flowTraffic["MaxSpoolBytes"] != float64(268435456) {
		t.Fatalf("unexpected FlowTraffic config: %#v", audit["FlowTraffic"])
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

func TestApplyAccessAuditConfigTaskDefaultsLocalSntpAccessOff(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	initial := []byte(`{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  },
  "Nodes": []
}`)
	if err := os.WriteFile(configPath, initial, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	task := panel.UpdateTask{
		TaskID: "access-audit-default-log-test",
		Type:   accessAuditTaskType,
		AccessAudit: &panel.AccessAuditTask{
			Enabled:       true,
			Endpoint:      "https://logs.sntp.uk/api/v1/access-events",
			Token:         "token",
			BatchSize:     1000,
			MaxQueueSize:  10000,
			FlushInterval: "1s",
			Timeout:       "5s",
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
	if logConfig["SNTPAccess"] != false {
		t.Fatalf("expected SNTPAccess to default false, got %#v", logConfig["SNTPAccess"])
	}
	audit, ok := config["AccessAudit"].(map[string]any)
	if !ok {
		t.Fatalf("missing AccessAudit config: %#v", config["AccessAudit"])
	}
	flowTraffic, ok := audit["FlowTraffic"].(map[string]any)
	if !ok || flowTraffic["Enabled"] != false || flowTraffic["CheckpointInterval"] != "5m" || flowTraffic["MaxSpoolAge"] != "24h" {
		t.Fatalf("unexpected default FlowTraffic config: %#v", audit["FlowTraffic"])
	}
	if audit["SpoolPath"] != "/var/lib/v2node/access-audit-spool/access.db" ||
		audit["MaxSpoolBytes"] != float64(1073741824) ||
		audit["MaxSpoolAge"] != "168h" {
		t.Fatalf("unexpected default access spool config: %#v", audit)
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

func TestAppendAccessAuditRuntimeStatusReportsCurrentConfig(t *testing.T) {
	controller := &Controller{
		conf: &conf.NodeConfig{
			APIHost: "https://panel.example",
			NodeID:  121,
		},
		server: &core.V2Core{
			Config: &conf.Conf{
				AccessAuditConfig: conf.AccessAuditConfig{
					Enabled:  true,
					Endpoint: " https://logs.sntp.uk/api/v1/access-events ",
					Token:    " secret ",
					FlowTraffic: conf.FlowTrafficConfig{
						Enabled: true,
					},
				},
			},
		},
	}

	status := panel.NodeRuntimeStatus{}
	controller.appendAccessAuditRuntimeStatus(&status)

	if !status.AccessAuditReported {
		t.Fatal("expected access audit runtime status to be reported")
	}
	if !status.AccessAuditEnabled {
		t.Fatal("expected access audit to be enabled")
	}
	if status.AccessAuditEndpoint != "https://logs.sntp.uk/api/v1/access-events" {
		t.Fatalf("unexpected access audit endpoint %q", status.AccessAuditEndpoint)
	}
	if !status.AccessAuditTokenConfigured {
		t.Fatal("expected token to be reported as configured")
	}
	if !status.FlowTrafficConfigReported || !status.FlowTrafficEnabled {
		t.Fatalf("expected flow traffic status to be reported: %#v", status)
	}
	if status.MachineInstanceID == "" {
		t.Fatal("expected machine instance id to be reported")
	}

	otherController := &Controller{
		conf: &conf.NodeConfig{
			APIHost: "https://panel.example",
			NodeID:  363,
		},
		server: controller.server,
	}
	otherStatus := panel.NodeRuntimeStatus{}
	otherController.appendAccessAuditRuntimeStatus(&otherStatus)
	if otherStatus.MachineInstanceID != status.MachineInstanceID {
		t.Fatalf("same process must report one machine identity across node ids: %q != %q", otherStatus.MachineInstanceID, status.MachineInstanceID)
	}
}

func TestAppendTLSRuntimeStatusReportsLocalCertFingerprint(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "trojan114.cer")
	keyPath := filepath.Join(dir, "trojan114.key")
	if err := generateSelfSslCertificate("tls.example.com", certPath, keyPath); err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("expected PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	sum := sha256.Sum256(cert.Raw)
	expectedFingerprint := hex.EncodeToString(sum[:])

	controller := &Controller{
		info: &panel.NodeInfo{
			Security: panel.Tls,
			Common: &panel.CommonNode{
				TlsSettings: panel.TlsSettings{
					ServerName: "tls.example.com",
				},
				CertInfo: &panel.CertInfo{
					CertFile:   certPath,
					CertDomain: "tls.example.com",
				},
			},
		},
	}

	status := panel.NodeRuntimeStatus{}
	controller.appendTLSRuntimeStatus(&status)

	if status.TLSCertSHA256 != expectedFingerprint {
		t.Fatalf("unexpected certificate fingerprint %q, want %q", status.TLSCertSHA256, expectedFingerprint)
	}
	if status.TLSCertFile != certPath {
		t.Fatalf("unexpected certificate file %q", status.TLSCertFile)
	}
	if status.TLSVerifyPeerCertByName != "tls.example.com" {
		t.Fatalf("unexpected verify name %q", status.TLSVerifyPeerCertByName)
	}
}
