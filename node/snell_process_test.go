package node

import (
	"strings"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestRenderSnellConfigSkipsObfsForVersion6(t *testing.T) {
	node := panel.ManagedSnellNode{
		ID:       1,
		Version:  6,
		Obfs:     "http",
		ObfsHost: "download.example.com",
	}
	credential := panel.ManagedSnellCredential{
		UserID: 1001,
		Port:   20001,
		PSK:    "secret",
	}

	config := renderSnellConfig(node, credential)

	for _, line := range []string{
		"listen = 0.0.0.0:20001,[::]:20001",
		"psk = secret",
		"version = 6",
	} {
		if !strings.Contains(config, line) {
			t.Fatalf("expected config to contain %q, got:\n%s", line, config)
		}
	}
	for _, line := range []string{"obfs = http", "obfs-host = download.example.com"} {
		if strings.Contains(config, line) {
			t.Fatalf("expected version 6 config to skip %q, got:\n%s", line, config)
		}
	}
}

func TestRenderSnellListenUsesExplicitListenIPWhenProvided(t *testing.T) {
	got := renderSnellListen("127.0.0.1", 20001, 6)
	if got != "127.0.0.1:20001" {
		t.Fatalf("renderSnellListen()=%q, want 127.0.0.1:20001", got)
	}
}

func TestSnellListenerKeyIncludesIdentity(t *testing.T) {
	got := snellListenerKey(1, 1001, 20001)
	const want = "snell-1-1001-20001"
	if got != want {
		t.Fatalf("snellListenerKey()=%q, want %q", got, want)
	}
}
