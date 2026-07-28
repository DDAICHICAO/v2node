package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
)

const offlineStateVersion = 1

type offlineState struct {
	Version      int                           `json:"version"`
	APIHost      string                        `json:"api_host"`
	NodeID       int                           `json:"node_id"`
	SavedAt      int64                         `json:"saved_at"`
	NodeInfo     *panel.NodeInfo               `json:"node_info"`
	Users        []panel.UserInfo              `json:"users"`
	Alive        map[int]int                   `json:"alive"`
	DeviceAlive  map[int]int                   `json:"device_alive"`
	UUIDIPFanout panel.UUIDIPFanoutGlobalState `json:"uuid_ip_fanout"`
	UserSyncSeq  int64                         `json:"user_sync_seq"`
}

type offlineStateStore struct {
	dir string
}

func newOfflineStateStore(dir string) *offlineStateStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = conf.DefaultStatePath
	}
	return &offlineStateStore{dir: dir}
}

func normalizeAPIHost(value string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(value)), "/")
}

func (s *offlineStateStore) pathFor(cfg conf.NodeConfig) string {
	identity := fmt.Sprintf("%s|%d", normalizeAPIHost(cfg.APIHost), cfg.NodeID)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(s.dir, fmt.Sprintf("node-%d-%s.json", cfg.NodeID, hex.EncodeToString(sum[:8])))
}

func validateOfflineState(cfg conf.NodeConfig, state *offlineState) error {
	if state == nil {
		return errors.New("offline state is nil")
	}
	if state.Version != offlineStateVersion {
		return fmt.Errorf("unsupported offline state version %d", state.Version)
	}
	if state.APIHost != normalizeAPIHost(cfg.APIHost) || state.NodeID != cfg.NodeID {
		return errors.New("offline state identity mismatch")
	}
	if state.SavedAt <= 0 || state.NodeInfo == nil || state.NodeInfo.Id != cfg.NodeID {
		return errors.New("offline state node info is incomplete")
	}
	if state.NodeInfo.Common == nil || state.NodeInfo.Common.BaseConfig == nil || state.NodeInfo.Tag == "" || state.NodeInfo.Type == "" {
		return errors.New("offline state runtime config is incomplete")
	}
	if state.Users == nil || state.Alive == nil || state.DeviceAlive == nil || state.UserSyncSeq < 0 {
		return errors.New("offline state user data is incomplete")
	}
	return nil
}

func (s *offlineStateStore) Load(cfg conf.NodeConfig) (*offlineState, error) {
	data, err := os.ReadFile(s.pathFor(cfg))
	if err != nil {
		return nil, err
	}
	var state offlineState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode offline state: %w", err)
	}
	if err := validateOfflineState(cfg, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *offlineStateStore) Save(cfg conf.NodeConfig, state *offlineState) error {
	if err := validateOfflineState(cfg, state); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create offline state directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0700); err != nil {
		return fmt.Errorf("protect offline state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode offline state: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".offline-state-*")
	if err != nil {
		return fmt.Errorf("create offline state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect offline state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write offline state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync offline state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close offline state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.pathFor(cfg)); err != nil {
		return fmt.Errorf("replace offline state: %w", err)
	}
	return nil
}

func cloneIntMap(input map[int]int) map[int]int {
	output := make(map[int]int, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
