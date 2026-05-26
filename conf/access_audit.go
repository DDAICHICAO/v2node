package conf

import (
	"fmt"
	"strings"
	"time"

	"github.com/wyx2685/v2node/common/accessaudit"
)

const (
	DefaultAccessAuditBatchSize     = 1000
	DefaultAccessAuditMaxQueueSize  = 10000
	DefaultAccessAuditFlushInterval = "1s"
	DefaultAccessAuditTimeout       = "5s"
)

type AccessAuditConfig struct {
	Enabled       bool   `mapstructure:"Enabled"`
	Endpoint      string `mapstructure:"Endpoint"`
	Token         string `mapstructure:"Token"`
	BatchSize     int    `mapstructure:"BatchSize"`
	MaxQueueSize  int    `mapstructure:"MaxQueueSize"`
	FlushInterval string `mapstructure:"FlushInterval"`
	Timeout       string `mapstructure:"Timeout"`
}

func (p *AccessAuditConfig) Normalize() error {
	p.Endpoint = strings.TrimSpace(p.Endpoint)
	p.Token = strings.TrimSpace(p.Token)
	p.FlushInterval = strings.TrimSpace(p.FlushInterval)
	p.Timeout = strings.TrimSpace(p.Timeout)
	if p.BatchSize <= 0 {
		p.BatchSize = DefaultAccessAuditBatchSize
	}
	if p.MaxQueueSize <= 0 {
		p.MaxQueueSize = DefaultAccessAuditMaxQueueSize
	}
	if p.FlushInterval == "" {
		p.FlushInterval = DefaultAccessAuditFlushInterval
	}
	if p.Timeout == "" {
		p.Timeout = DefaultAccessAuditTimeout
	}
	if !p.Enabled {
		return nil
	}
	if p.Endpoint == "" {
		return fmt.Errorf("AccessAudit.Endpoint is required when AccessAudit.Enabled is true")
	}
	if p.Token == "" {
		return fmt.Errorf("AccessAudit.Token is required when AccessAudit.Enabled is true")
	}
	if _, err := time.ParseDuration(p.FlushInterval); err != nil {
		return fmt.Errorf("parse AccessAudit.FlushInterval: %w", err)
	}
	if _, err := time.ParseDuration(p.Timeout); err != nil {
		return fmt.Errorf("parse AccessAudit.Timeout: %w", err)
	}
	return nil
}

func (p AccessAuditConfig) RuntimeConfig() (accessaudit.Config, error) {
	if !p.Enabled {
		return accessaudit.Config{Enabled: false}, nil
	}
	flushInterval, err := time.ParseDuration(p.FlushInterval)
	if err != nil {
		return accessaudit.Config{}, fmt.Errorf("parse AccessAudit.FlushInterval: %w", err)
	}
	timeout, err := time.ParseDuration(p.Timeout)
	if err != nil {
		return accessaudit.Config{}, fmt.Errorf("parse AccessAudit.Timeout: %w", err)
	}
	return accessaudit.Config{
		Enabled:       p.Enabled,
		Endpoint:      p.Endpoint,
		Token:         p.Token,
		BatchSize:     p.BatchSize,
		MaxQueueSize:  p.MaxQueueSize,
		FlushInterval: flushInterval,
		Timeout:       timeout,
	}, nil
}
