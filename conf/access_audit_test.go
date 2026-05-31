package conf

import (
	"testing"
	"time"
)

func TestAccessAuditNormalizeDefaultsWhenDisabled(t *testing.T) {
	cfg := AccessAuditConfig{}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize disabled config: %v", err)
	}
	if cfg.Enabled {
		t.Fatal("expected disabled by default")
	}
	if cfg.BatchSize != DefaultAccessAuditBatchSize {
		t.Fatalf("expected default batch size, got %d", cfg.BatchSize)
	}
	if cfg.MaxQueueSize != DefaultAccessAuditMaxQueueSize {
		t.Fatalf("expected default queue size, got %d", cfg.MaxQueueSize)
	}
	if cfg.FlushInterval != DefaultAccessAuditFlushInterval {
		t.Fatalf("expected default flush interval, got %q", cfg.FlushInterval)
	}
	if cfg.Timeout != DefaultAccessAuditTimeout {
		t.Fatalf("expected default timeout, got %q", cfg.Timeout)
	}
}

func TestNewDefaultsDisableLocalSntpAccessLog(t *testing.T) {
	cfg := New()
	if cfg.LogConfig.SNTPAccess {
		t.Fatal("expected local SNTP access log to be disabled by default")
	}
}

func TestAccessAuditNormalizeRequiresEndpointAndTokenWhenEnabled(t *testing.T) {
	cfg := AccessAuditConfig{Enabled: true, Endpoint: "https://logs.sntp.uk/api/v1/access-events"}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected missing token to fail")
	}

	cfg = AccessAuditConfig{Enabled: true, Token: "secret"}
	if err := cfg.Normalize(); err == nil {
		t.Fatal("expected missing endpoint to fail")
	}
}

func TestAccessAuditRuntimeConfigParsesDurations(t *testing.T) {
	cfg := AccessAuditConfig{
		Enabled:       true,
		Endpoint:      " https://logs.sntp.uk/api/v1/access-events ",
		Token:         " secret ",
		BatchSize:     200,
		MaxQueueSize:  500,
		FlushInterval: "2s",
		Timeout:       "3s",
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize enabled config: %v", err)
	}
	runtimeCfg, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config: %v", err)
	}
	if runtimeCfg.Endpoint != "https://logs.sntp.uk/api/v1/access-events" {
		t.Fatalf("unexpected endpoint %q", runtimeCfg.Endpoint)
	}
	if runtimeCfg.Token != "secret" {
		t.Fatalf("unexpected token %q", runtimeCfg.Token)
	}
	if runtimeCfg.FlushInterval != 2*time.Second {
		t.Fatalf("expected 2s flush interval, got %s", runtimeCfg.FlushInterval)
	}
	if runtimeCfg.Timeout != 3*time.Second {
		t.Fatalf("expected 3s timeout, got %s", runtimeCfg.Timeout)
	}
}

func TestAccessAuditRuntimeConfigIgnoresDisabledDurations(t *testing.T) {
	cfg := AccessAuditConfig{
		Enabled:       false,
		FlushInterval: "not-a-duration",
		Timeout:       "also-not-a-duration",
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("normalize disabled config: %v", err)
	}
	runtimeCfg, err := cfg.RuntimeConfig()
	if err != nil {
		t.Fatalf("runtime config should ignore disabled durations: %v", err)
	}
	if runtimeCfg.Enabled {
		t.Fatal("expected disabled runtime config")
	}
}
