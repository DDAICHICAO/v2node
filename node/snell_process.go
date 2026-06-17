package node

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
)

type snellProcess struct {
	Key        string
	SnellID    int
	UserID     int
	Port       int
	ConfigPath string
	Command    *exec.Cmd
}

func snellListenerKey(snellID, userID, port int) string {
	return fmt.Sprintf("snell-%d-%d-%d", snellID, userID, port)
}

func renderSnellConfig(node panel.ManagedSnellNode, credential panel.ManagedSnellCredential) string {
	listenIP := strings.TrimSpace(node.ListenIP)
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	version := node.Version
	if version <= 0 {
		version = 6
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "listen = %s\n", net.JoinHostPort(listenIP, strconv.Itoa(credential.Port)))
	fmt.Fprintf(&builder, "psk = %s\n", credential.PSK)
	fmt.Fprintf(&builder, "version = %d\n", version)
	if obfs := strings.TrimSpace(node.Obfs); obfs != "" {
		fmt.Fprintf(&builder, "obfs = %s\n", obfs)
	}
	if obfsHost := strings.TrimSpace(node.ObfsHost); obfsHost != "" {
		fmt.Fprintf(&builder, "obfs-host = %s\n", obfsHost)
	}
	return builder.String()
}

func writeSnellConfig(configDir string, node panel.ManagedSnellNode, credential panel.ManagedSnellCredential) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", fmt.Errorf("snell config dir is required")
	}
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return "", fmt.Errorf("create snell config dir: %w", err)
	}

	configPath := filepath.Join(configDir, snellListenerKey(node.ID, credential.UserID, credential.Port)+".conf")
	tmp, err := os.CreateTemp(configDir, "."+filepath.Base(configPath)+".*")
	if err != nil {
		return "", fmt.Errorf("create snell config temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.WriteString(renderSnellConfig(node, credential)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write snell config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close snell config temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		return "", fmt.Errorf("chmod snell config temp file: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		return "", fmt.Errorf("replace snell config: %w", err)
	}
	return configPath, nil
}

func startSnellProcess(binary, configPath string) (*exec.Cmd, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "snell-server"
	}
	cmd := exec.Command(binary, "-c", configPath)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func stopSnellProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.ProcessState != nil {
		return nil
	}

	killErr := cmd.Process.Kill()
	waitErr := cmd.Wait()
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			waitErr = nil
		}
	}
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return killErr
	}
	return waitErr
}
