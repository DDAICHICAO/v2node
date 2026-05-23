package conf

import (
	"os"
	"strings"
	"testing"
)

func TestNormalizeLogConfigFilePreservesNodesAndExtraLogFields(t *testing.T) {
	path := writeTempConfig(t, `{
    "Log": {
        "Level": "info",
        "Output": "/tmp/noisy.log",
        "Access": "console",
        "SNTPAccess": false
    },
    "Nodes": [
        {
            "ApiHost": "http://anode.sntp.uk:999",
            "NodeID": 514,
            "ApiKey": "KEEP_ME_1",
            "Timeout": 15
        },
        {
            "ApiHost": "http://another.sntp.uk:999",
            "NodeID": 999,
            "ApiKey": "KEEP_ME_2",
            "Timeout": 30
        }
    ]
}
`)

	changed, err := NormalizeLogConfigFile(path)
	if err != nil {
		t.Fatalf("NormalizeLogConfigFile() error = %v", err)
	}
	if !changed {
		t.Fatal("NormalizeLogConfigFile() changed = false, want true")
	}

	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(gotBytes)

	for _, want := range []string{
		`"Level": "warning"`,
		`"Output": ""`,
		`"Access": "none"`,
		`"SNTPAccess": false`,
		`"ApiHost": "http://anode.sntp.uk:999"`,
		`"NodeID": 514`,
		`"ApiKey": "KEEP_ME_1"`,
		`"ApiKey": "KEEP_ME_2"`,
		`"Timeout": 30`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("normalized config missing %s:\n%s", want, got)
		}
	}
	if count := strings.Count(got, `"Log"`); count != 1 {
		t.Fatalf("normalized config has %d Log sections, want 1:\n%s", count, got)
	}
}

func TestNormalizeLogConfigFileAlreadyNormalized(t *testing.T) {
	path := writeTempConfig(t, `{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none",
        "SNTPAccess": false
    },
    "Nodes": []
}
`)

	changed, err := NormalizeLogConfigFile(path)
	if err != nil {
		t.Fatalf("NormalizeLogConfigFile() error = %v", err)
	}
	if changed {
		t.Fatal("NormalizeLogConfigFile() changed = true, want false")
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/config.json"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
