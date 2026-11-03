package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/wyx2685/v2node/common/instance"
	"github.com/wyx2685/v2node/conf"
)

// ConfigError keeps attribution through preparation, activation and rollback.
// Only Stage and Code are sent to the panel; Err stays in local diagnostics.
type ConfigError struct {
	APIHost string
	NodeID  int
	Stage   string
	Code    string
	Err     error
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("node %d %s (%s): %v", e.NodeID, e.Stage, e.Code, e.Err)
}
func (e *ConfigError) Unwrap() error { return e.Err }

func newConfigError(cfg conf.NodeConfig, stage, code string, err error) error {
	var existing *ConfigError
	if errors.As(err, &existing) {
		return err
	}
	var certErr *certificateError
	if errors.As(err, &certErr) {
		stage, code = "certificate", certErr.code
	}
	return &ConfigError{APIHost: cfg.APIHost, NodeID: cfg.NodeID, Stage: stage, Code: code, Err: err}
}

type configStatusReport struct {
	config         conf.NodeConfig
	State          string `json:"state"`
	Stage          string `json:"stage,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	RuntimeServing bool   `json:"runtime_serving"`
	NextRetryAt    int64  `json:"next_retry_at"`
}

func configStatusReports(configs []conf.NodeConfig, active *Node, err error, next time.Time) []configStatusReport {
	var failure *ConfigError
	errors.As(err, &failure)
	rows := make([]configStatusReport, 0, len(configs))
	for _, cfg := range configs {
		row := configStatusReport{config: cfg, State: "applied"}
		if active != nil {
			for _, c := range active.controllers {
				if c.conf.NodeID == cfg.NodeID && normalizeAPIHost(c.conf.APIHost) == normalizeAPIHost(cfg.APIHost) && !c.deleted.Load() {
					row.RuntimeServing = c.runtime.Started()
					break
				}
			}
		}
		if err != nil {
			row.State, row.Stage, row.ErrorCode = "failed", "configuration", "configuration_failed"
			if !next.IsZero() {
				row.NextRetryAt = next.Unix()
			}
			if failure != nil {
				if failure.NodeID == cfg.NodeID && normalizeAPIHost(failure.APIHost) == normalizeAPIHost(cfg.APIHost) {
					row.Stage, row.ErrorCode = failure.Stage, failure.Code
				} else {
					row.State, row.ErrorCode = "blocked", "configuration_blocked"
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// ConfigStatusReporter runs independently of business controllers, including nodes
// that failed before opening a listener. It never writes runtime/traffic status.
type ConfigStatusReporter struct {
	updates chan []configStatusReport
	client  *http.Client
}

func NewConfigStatusReporter() *ConfigStatusReporter {
	return &ConfigStatusReporter{
		updates: make(chan []configStatusReport, 1),
		client:  &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// Update is called only by the server owner loop. Replace queued stale generations.
func (r *ConfigStatusReporter) Update(configs []conf.NodeConfig, active *Node, err error, next time.Time) {
	rows := configStatusReports(configs, active, err, next)
	select {
	case <-r.updates:
	default:
	}
	r.updates <- rows
}

func (r *ConfigStatusReporter) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var rows []configStatusReport
	for {
		select {
		case <-ctx.Done():
			return
		case rows = <-r.updates:
		case <-ticker.C:
		}
		var wg sync.WaitGroup
		slots := make(chan struct{}, 4)
		for _, row := range rows {
			if ctx.Err() != nil {
				return
			}
			slots <- struct{}{}
			wg.Go(func() {
				defer func() { <-slots }()
				if err := r.report(ctx, row); err != nil {
					// HTTP bodies, panel addresses and authentication material stay private.
					log.WithField("node_id", row.config.NodeID).Debug("SNTP configuration status report deferred")
				}
			})
		}
		wg.Wait()
	}
}

func (r *ConfigStatusReporter) report(ctx context.Context, row configStatusReport) error {
	cfg := row.config
	u, err := url.Parse(cfg.APIHost)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid configuration status endpoint")
	}
	hostname, _ := os.Hostname()
	body := struct {
		configStatusReport
		NodeID     int    `json:"node_id"`
		Token      string `json:"token"`
		InstanceID string `json:"instance_id"`
		MachineID  string `json:"machine_instance_id"`
		MachineIP  string `json:"machine_ip,omitempty"`
		Hostname   string `json:"hostname"`
	}{row, cfg.NodeID, cfg.Key, instance.ResolveID(cfg.APIHost, cfg.NodeID), instance.ResolveMachineID(cfg.APIHost), cfg.MachineIP, hostname}
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("invalid configuration status payload")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.APIHost, "/")+"/api/v2/server/config-status", bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid configuration status request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("configuration status transport unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("configuration status HTTP %d", resp.StatusCode)
	}
	var result struct {
		Data bool `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result) != nil || !result.Data {
		return errors.New("configuration status rejected")
	}
	return nil
}
