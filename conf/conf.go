package conf

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

const DefaultNodeRetryCount = 1
const DefaultNodeTimeout = 15

type Conf struct {
	LogConfig         LogConfig         `mapstructure:"Log"`
	AccessAuditConfig AccessAuditConfig `mapstructure:"AccessAudit"`
	NodeConfigs       []NodeConfig      `mapstructure:"Nodes"`
	PprofPort         int               `mapstructure:"PprofPort"`
}

type LogConfig struct {
	Level      string `mapstructure:"Level"`
	Output     string `mapstructure:"Output"`
	Access     string `mapstructure:"Access"`
	SNTPAccess bool   `mapstructure:"SNTPAccess"`
}

type NodeConfig struct {
	APIHost                 string `mapstructure:"ApiHost"`
	NodeID                  int    `mapstructure:"NodeID"`
	Key                     string `mapstructure:"ApiKey"`
	Timeout                 int    `mapstructure:"Timeout"`
	RetryCount              *int   `mapstructure:"RetryCount"`
	AppTransportTokenSecret string `mapstructure:"AppTransportTokenSecret"`
}

func New() *Conf {
	return &Conf{
		LogConfig: LogConfig{
			Level:      "warning",
			Output:     "",
			Access:     "none",
			SNTPAccess: true,
		},
		AccessAuditConfig: AccessAuditConfig{
			BatchSize:     DefaultAccessAuditBatchSize,
			MaxQueueSize:  DefaultAccessAuditMaxQueueSize,
			FlushInterval: DefaultAccessAuditFlushInterval,
			Timeout:       DefaultAccessAuditTimeout,
		},
	}
}

func (p *Conf) LoadFromPath(filePath string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open config file error: %s", err)
	}
	defer f.Close()
	v := viper.New()
	v.SetConfigFile(filePath)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file error: %s", err)
	}
	if err := v.Unmarshal(p); err != nil {
		return fmt.Errorf("unmarshal config error: %s", err)
	}
	p.LogConfig.Normalize()
	if err := p.AccessAuditConfig.Normalize(); err != nil {
		return err
	}
	for i := range p.NodeConfigs {
		if p.NodeConfigs[i].RetryCount == nil {
			p.NodeConfigs[i].RetryCount = intPtr(DefaultNodeRetryCount)
		}
	}
	return nil
}

func (p *LogConfig) Normalize() {
	p.Level = strings.ToLower(strings.TrimSpace(p.Level))
	if p.Level == "" {
		p.Level = "warning"
	}
	p.Output = strings.TrimSpace(p.Output)
	p.Access = strings.TrimSpace(p.Access)
	if p.Access == "" {
		p.Access = "none"
	}
}

func (p LogConfig) CoreAccessLog() string {
	switch strings.ToLower(strings.TrimSpace(p.Access)) {
	case "", "none":
		return "none"
	case "console":
		return ""
	default:
		return p.Access
	}
}

func intPtr(v int) *int {
	return &v
}
