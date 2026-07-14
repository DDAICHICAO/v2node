package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewDefaultsOfflineStatePath(t *testing.T) {
	c := New()
	if c.StatePath != DefaultStatePath {
		t.Fatalf("StatePath=%q, want %q", c.StatePath, DefaultStatePath)
	}
}

func TestLoadFromPathTrimsOfflineStatePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"StatePath":"  /tmp/v2node-state  "}`), 0600); err != nil {
		t.Fatal(err)
	}

	c := New()
	if err := c.LoadFromPath(path); err != nil {
		t.Fatal(err)
	}
	if c.StatePath != "/tmp/v2node-state" {
		t.Fatalf("StatePath=%q", c.StatePath)
	}
}
