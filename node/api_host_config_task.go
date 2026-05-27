package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

const apiHostTaskType = "api_host"

var apiHostConfigPath = "/etc/v2node/config.json"
var errApiHostNoChange = errors.New("api host config already up to date")

func updateTaskIsApiHostConfig(task panel.UpdateTask) bool {
	return strings.EqualFold(strings.TrimSpace(task.Type), apiHostTaskType) || task.ApiHost != nil
}

func (c *Controller) runApiHostConfigTask(task panel.UpdateTask) {
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

	c.reportUpdateStatus(task, updateStatusInstalling, "applying api host config")
	if err := applyApiHostConfigTask(task, apiHostConfigPath); err != nil {
		if errors.Is(err, errApiHostNoChange) {
			c.markUpdateTaskSkipped(task, err.Error())
			return
		}
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"task": task.TaskID,
			"err":  err,
		}).Error("Apply api host config failed")
		c.reportUpdateStatus(task, updateStatusFailed, err.Error())
		return
	}

	state := updateState{
		TaskID:    task.TaskID,
		Version:   task.Version,
		Status:    updateStatusSuccess,
		Message:   "api host config applied, restarting service",
		UpdatedAt: time.Now().Unix(),
	}
	if err := writeUpdateState(state); err != nil {
		log.WithField("err", err).Error("Write api host update state failed")
	}

	c.reportUpdateStatus(task, updateStatusSuccess, "api host config applied, restarting service")
	if err := scheduleServiceRestart(); err != nil {
		log.WithFields(log.Fields{
			"tag":  c.tag,
			"task": task.TaskID,
			"err":  err,
		}).Error("Restart after api host config failed")
	}
}

func applyApiHostConfigTask(task panel.UpdateTask, configPath string) error {
	apiHost := task.ApiHost
	if apiHost == nil {
		return fmt.Errorf("api_host payload is required")
	}
	normalized, err := normalizeApiHostTask(*apiHost)
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
		return fmt.Errorf("config json is empty")
	}

	nodes, ok := config["Nodes"].([]any)
	if !ok || len(nodes) == 0 {
		return fmt.Errorf("config Nodes is empty")
	}

	changed := 0
	matched := 0
	for _, rawNode := range nodes {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		current, ok := node["ApiHost"].(string)
		if !ok {
			continue
		}
		if normalized.MatchApiHost != "" && !apiHostMatches(current, normalized.MatchApiHost) {
			continue
		}
		matched++
		if strings.TrimRight(strings.TrimSpace(current), "/") == normalized.ApiHost {
			continue
		}
		node["ApiHost"] = normalized.ApiHost
		changed++
	}
	if changed == 0 {
		if normalized.MatchApiHost != "" && matched == 0 {
			return fmt.Errorf("no Nodes ApiHost matched %s", normalized.MatchApiHost)
		}
		return errApiHostNoChange
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

func normalizeApiHostTask(task panel.ApiHostTask) (panel.ApiHostTask, error) {
	apiHost, err := normalizeApiHostURL(task.ApiHost, "api_host.api_host")
	if err != nil {
		return task, err
	}
	task.ApiHost = apiHost

	task.MatchApiHost = strings.TrimSpace(task.MatchApiHost)
	if task.MatchApiHost != "" {
		matchApiHost, err := normalizeApiHostURL(task.MatchApiHost, "api_host.match_api_host")
		if err != nil {
			return task, err
		}
		task.MatchApiHost = matchApiHost
	}
	return task, nil
}

func normalizeApiHostURL(value string, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%s must be http(s) url", field)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("%s must be root http(s) url without path, query, or fragment", field)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func apiHostMatches(current string, expected string) bool {
	normalizedCurrent, err := normalizeApiHostURL(current, "current ApiHost")
	if err != nil {
		return strings.TrimRight(strings.TrimSpace(current), "/") == expected
	}
	return normalizedCurrent == expected
}
