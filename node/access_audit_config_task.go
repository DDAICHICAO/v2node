package node

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

const accessAuditTaskType = "access_audit"

var accessAuditConfigPath = "/etc/v2node/config.json"

func updateTaskIsAccessAuditConfig(task panel.UpdateTask) bool {
	return strings.EqualFold(strings.TrimSpace(task.Type), accessAuditTaskType) || task.AccessAudit != nil
}

func (c *Controller) runAccessAuditConfigTask(task panel.UpdateTask) {
	if !updateRunMu.TryLock() {
		c.reportUpdateStatus(task, updateStatusSkipped, "another update task is already running")
		return
	}
	defer updateRunMu.Unlock()

	releaseLock, acquired, err := acquireUpdateLock()
	if err != nil {
		c.reportUpdateStatus(task, updateStatusFailed, err.Error())
		return
	}
	if !acquired {
		c.reportUpdateStatus(task, updateStatusSkipped, "another v2node process is updating")
		return
	}
	defer releaseLock()

	if updateTaskApplied(task.TaskID) {
		return
	}

	c.reportUpdateStatus(task, updateStatusInstalling, "applying access audit config")
	if err := applyAccessAuditConfigTask(task, accessAuditConfigPath); err != nil {
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"task": task.TaskID,
			"err":  err,
		}).Error("Apply access audit config failed")
		c.reportUpdateStatus(task, updateStatusFailed, err.Error())
		return
	}

	state := updateState{
		TaskID:    task.TaskID,
		Version:   task.Version,
		Status:    updateStatusSuccess,
		Message:   "access audit config applied, restarting service",
		UpdatedAt: time.Now().Unix(),
	}
	if err := writeUpdateState(state); err != nil {
		log.WithField("err", err).Error("Write access audit update state failed")
	}

	c.reportUpdateStatus(task, updateStatusSuccess, "access audit config applied, restarting service")
	if err := scheduleServiceRestart(); err != nil {
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"task": task.TaskID,
			"err":  err,
		}).Error("Restart after access audit config failed")
	}
}

func applyAccessAuditConfigTask(task panel.UpdateTask, configPath string) error {
	audit := task.AccessAudit
	if audit == nil {
		return fmt.Errorf("access_audit payload is required")
	}
	normalized, err := normalizeAccessAuditTask(*audit)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode config json: %w", err)
	}
	if config == nil {
		config = make(map[string]any)
	}

	logConfig, _ := config["Log"].(map[string]any)
	if logConfig == nil {
		logConfig = make(map[string]any)
		config["Log"] = logConfig
	}
	if _, ok := logConfig["Level"]; !ok {
		logConfig["Level"] = "warning"
	}
	if _, ok := logConfig["Output"]; !ok {
		logConfig["Output"] = ""
	}
	if _, ok := logConfig["Access"]; !ok {
		logConfig["Access"] = "none"
	}
	if normalized.SNTPAccess != nil {
		logConfig["SNTPAccess"] = *normalized.SNTPAccess
	} else {
		logConfig["SNTPAccess"] = false
	}

	config["AccessAudit"] = map[string]any{
		"Enabled":       normalized.Enabled,
		"Endpoint":      normalized.Endpoint,
		"Token":         normalized.Token,
		"BatchSize":     normalized.BatchSize,
		"MaxQueueSize":  normalized.MaxQueueSize,
		"FlushInterval": normalized.FlushInterval,
		"Timeout":       normalized.Timeout,
	}

	output, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("encode config json: %w", err)
	}
	output = append(output, '\n')

	backupPath := fmt.Sprintf("%s.bak.%d", configPath, time.Now().Unix())
	if err := os.WriteFile(backupPath, data, 0600); err != nil {
		return fmt.Errorf("write config backup: %w", err)
	}

	dir := filepath.Dir(configPath)
	tmp, err := os.CreateTemp(dir, ".config.json.*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(output); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func normalizeAccessAuditTask(task panel.AccessAuditTask) (panel.AccessAuditTask, error) {
	task.Endpoint = strings.TrimSpace(task.Endpoint)
	task.Token = strings.TrimSpace(task.Token)
	task.FlushInterval = strings.TrimSpace(task.FlushInterval)
	task.Timeout = strings.TrimSpace(task.Timeout)

	if task.Enabled {
		if task.Endpoint == "" {
			return task, fmt.Errorf("access_audit.endpoint is required")
		}
		parsed, err := url.Parse(task.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return task, fmt.Errorf("access_audit.endpoint must be http(s) url")
		}
		if task.Token == "" {
			return task, fmt.Errorf("access_audit.token is required")
		}
	}
	if task.BatchSize <= 0 {
		task.BatchSize = 1000
	}
	if task.MaxQueueSize <= 0 {
		task.MaxQueueSize = 10000
	}
	if task.FlushInterval == "" {
		task.FlushInterval = "1s"
	}
	if task.Timeout == "" {
		task.Timeout = "5s"
	}
	if _, err := time.ParseDuration(task.FlushInterval); err != nil {
		return task, fmt.Errorf("parse access_audit.flush_interval: %w", err)
	}
	if _, err := time.ParseDuration(task.Timeout); err != nil {
		return task, fmt.Errorf("parse access_audit.timeout: %w", err)
	}
	return task, nil
}
