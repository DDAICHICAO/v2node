package node

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"encoding/json/v2"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	selfversion "github.com/wyx2685/v2node/common/version"
)

const (
	updateStatusDownloading = "downloading"
	updateStatusInstalling  = "installing"
	updateStatusSuccess     = "success"
	updateStatusFailed      = "failed"
	updateStatusSkipped     = "skipped"
)

var updateRunMu sync.Mutex

type updateState struct {
	TaskID    string `json:"task_id"`
	Version   string `json:"version"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	UpdatedAt int64  `json:"updated_at"`
}

type extractedUpdateFiles struct {
	Binary  string
	GeoIP   string
	GeoSite string
}

func (c *Controller) checkUpdateTask(ctx context.Context) {
	task, err := c.apiClient.GetUpdateTask(ctx)
	if err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get update task failed")
		return
	}
	if task == nil || !task.Enabled || task.TaskID == "" || task.Version == "" {
		return
	}
	if updateTaskApplied(task.TaskID) {
		return
	}
	if currentVersion := localVersion(); updateTaskIsDowngrade(currentVersion, task.Version) {
		c.reportUpdateStatus(*task, updateStatusSkipped, "target version is older than current version "+currentVersion)
		return
	}

	go c.runUpdateTask(*task)
}

func (c *Controller) runUpdateTask(task panel.UpdateTask) {
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

	log.WithFields(log.Fields{
		"task":    task.TaskID,
		"version": task.Version,
	}).Info("Start v2node update")

	c.reportUpdateStatus(task, updateStatusDownloading, "downloading update package")
	if err := performUpdate(task, func(status, message string) {
		c.reportUpdateStatus(task, status, message)
	}); err != nil {
		log.WithFields(log.Fields{
			"task": task.TaskID,
			"err":  err,
		}).Error("Update v2node failed")
		c.reportUpdateStatus(task, updateStatusFailed, err.Error())
		return
	}

	if err := scheduleServiceRestart(); err != nil {
		c.reportUpdateStatus(task, updateStatusFailed, "installed but restart failed: "+err.Error())
		return
	}

	state := updateState{
		TaskID:    task.TaskID,
		Version:   task.Version,
		Status:    updateStatusSuccess,
		Message:   "installed, restarting service",
		UpdatedAt: time.Now().Unix(),
	}
	if err := writeUpdateState(state); err != nil {
		log.WithField("err", err).Error("Write update state failed")
	}

	c.reportUpdateStatus(task, updateStatusSuccess, "installed, restarting service")
}

func (c *Controller) reportUpdateStatus(task panel.UpdateTask, status string, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.apiClient.ReportUpdateStatus(ctx, panel.UpdateReport{
		TaskID:         task.TaskID,
		Version:        task.Version,
		CurrentVersion: localVersion(),
		Status:         status,
		Message:        message,
	})
	if err != nil {
		log.WithFields(log.Fields{
			"tag":    c.tag,
			"task":   task.TaskID,
			"status": status,
			"err":    err,
		}).Error("Report update status failed")
	}
}

func performUpdate(task panel.UpdateTask, report func(status, message string)) error {
	tmpDir := filepath.Join(os.TempDir(), "v2node-update-"+safeFilePart(task.TaskID))
	if err := os.RemoveAll(tmpDir); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	downloadURL := strings.TrimSpace(task.DownloadURL)
	if downloadURL == "" {
		downloadURL = defaultUpdateDownloadURL(task.Version)
	}

	archivePath := filepath.Join(tmpDir, "v2node-update.zip")
	if err := downloadFile(downloadURL, archivePath); err != nil {
		return err
	}
	if task.SHA256 != "" {
		if err := verifySHA256(archivePath, task.SHA256); err != nil {
			return err
		}
	}

	report(updateStatusInstalling, "extracting update package")
	files, err := extractUpdateZip(archivePath, filepath.Join(tmpDir, "extract"))
	if err != nil {
		return err
	}
	if files.Binary == "" {
		return fmt.Errorf("update package does not contain v2node binary")
	}

	targetBinary := installedBinaryPath()
	if err := installBinary(files.Binary, targetBinary, task.TaskID); err != nil {
		return err
	}
	if err := installDataFiles(files, filepath.Dir(targetBinary)); err != nil {
		return err
	}
	return nil
}

func downloadFile(url string, dest string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed: http status %d", resp.StatusCode)
	}

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func defaultUpdateDownloadURL(version string) string {
	return fmt.Sprintf(
		"https://github.com/DDAICHICAO/v2node/releases/download/%s/v2node-linux-%s.zip",
		strings.TrimSpace(version),
		updateReleaseArch(),
	)
}

func updateReleaseArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "64"
	case "arm64":
		return "arm64-v8a"
	case "s390x":
		return "s390x"
	default:
		return "64"
	}
}

func verifySHA256(path string, expected string) error {
	expected = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expected)), "sha256:")
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

func extractUpdateZip(archivePath string, destDir string) (extractedUpdateFiles, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return extractedUpdateFiles{}, err
	}
	defer reader.Close()

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return extractedUpdateFiles{}, err
	}

	var files extractedUpdateFiles
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(file.Name)
		if base != "v2node" && base != "geoip.dat" && base != "geosite.dat" {
			continue
		}

		src, err := file.Open()
		if err != nil {
			return files, err
		}
		target := filepath.Join(destDir, base)
		mode := file.FileInfo().Mode()
		if base == "v2node" {
			mode = 0755
		}
		if err := writeFromReader(target, src, mode); err != nil {
			src.Close()
			return files, err
		}
		src.Close()

		switch base {
		case "v2node":
			files.Binary = target
		case "geoip.dat":
			files.GeoIP = target
		case "geosite.dat":
			files.GeoSite = target
		}
	}
	return files, nil
}

func writeFromReader(target string, src io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(target, mode)
}

func installBinary(source string, target string, taskID string) error {
	if target == "" {
		return fmt.Errorf("target binary path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}

	if _, err := os.Stat(target); err == nil {
		backup := target + ".bak." + safeFilePart(taskID)
		if err := copyFile(target, backup, 0755); err != nil {
			return fmt.Errorf("backup current binary: %w", err)
		}
	}

	newPath := target + ".new." + safeFilePart(taskID)
	if err := copyFile(source, newPath, 0755); err != nil {
		return err
	}
	if err := os.Rename(newPath, target); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("replace binary: %w", err)
	}
	return os.Chmod(target, 0755)
}

func installDataFiles(files extractedUpdateFiles, binaryDir string) error {
	dirs := []string{"/etc/v2node", binaryDir}
	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, ok := seen[dir]; ok {
			continue
		}
		seen[dir] = struct{}{}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if files.GeoIP != "" {
			if err := copyFile(files.GeoIP, filepath.Join(dir, "geoip.dat"), 0644); err != nil {
				return err
			}
		}
		if files.GeoSite != "" {
			if err := copyFile(files.GeoSite, filepath.Join(dir, "geosite.dat"), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(source string, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFromReader(target, in, mode)
}

func installedBinaryPath() string {
	if runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			return exe
		}
		return "v2node.exe"
	}
	const defaultPath = "/usr/local/v2node/v2node"
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return defaultPath
}

func scheduleServiceRestart() error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("automatic restart is not supported on windows")
	}
	script := `(sleep 1; if command -v systemctl >/dev/null 2>&1; then systemctl restart v2node; else service v2node restart; fi) >/dev/null 2>&1 &`
	return exec.Command("sh", "-c", script).Start()
}

func localVersion() string {
	if currentVersion := selfversion.FromCommand(installedBinaryPath()); currentVersion != "" {
		return currentVersion
	}

	return selfversion.Current()
}

func updateTaskApplied(taskID string) bool {
	state, err := readUpdateState()
	return err == nil && state.TaskID == taskID && state.Status == updateStatusSuccess
}

func updateTaskIsDowngrade(currentVersion string, targetVersion string) bool {
	current, ok := comparableUpdateVersion(currentVersion)
	if !ok {
		return false
	}
	target, ok := comparableUpdateVersion(targetVersion)
	if !ok {
		return false
	}

	maxLen := len(current)
	if len(target) > maxLen {
		maxLen = len(target)
	}
	for i := 0; i < maxLen; i++ {
		currentPart := 0
		if i < len(current) {
			currentPart = current[i]
		}
		targetPart := 0
		if i < len(target) {
			targetPart = target[i]
		}
		if targetPart < currentPart {
			return true
		}
		if targetPart > currentPart {
			return false
		}
	}
	return false
}

func comparableUpdateVersion(version string) ([]int, bool) {
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, false
	}
	fields := strings.Fields(version)
	if len(fields) >= 2 && strings.EqualFold(fields[0], "v2node") {
		version = strings.TrimSpace(fields[1])
	}
	version = strings.TrimLeft(version, "vV")
	parts := strings.Split(version, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return nil, false
	}

	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, false
			}
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		numbers = append(numbers, value)
	}
	return numbers, true
}

func updateStatePath() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.TempDir(), "v2node-update-state.json")
	}
	return "/etc/v2node/update_state.json"
}

func readUpdateState() (updateState, error) {
	data, err := os.ReadFile(updateStatePath())
	if err != nil {
		return updateState{}, err
	}
	var state updateState
	if err := json.Unmarshal(data, &state); err != nil {
		return updateState{}, err
	}
	return state, nil
}

func writeUpdateState(state updateState) error {
	path := updateStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func acquireUpdateLock() (func(), bool, error) {
	lockPath := filepath.Join(os.TempDir(), "v2node-updater.lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			info, statErr := os.Stat(lockPath)
			if statErr == nil && time.Since(info.ModTime()) > 30*time.Minute {
				_ = os.Remove(lockPath)
				return acquireUpdateLock()
			}
			return func() {}, false, nil
		}
		return func() {}, false, err
	}

	_, _ = fmt.Fprintf(file, "%d\n%d\n", os.Getpid(), time.Now().Unix())
	release := func() {
		_ = file.Close()
		_ = os.Remove(lockPath)
	}
	return release, true, nil
}

func safeFilePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "default"
	}
	var builder strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			builder.WriteRune(r)
		}
	}
	if builder.Len() == 0 {
		return "default"
	}
	return builder.String()
}
