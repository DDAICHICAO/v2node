package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/conf"
	"github.com/wyx2685/v2node/core"
)

const routeRuntimeStateVersion = 1

type routeRuntimeState struct {
	Version    int                 `json:"version"`
	Generation uint64              `json:"generation"`
	SavedAt    int64               `json:"saved_at"`
	VectorHash string              `json:"vector_hash"`
	Entries    []routeRuntimeEntry `json:"entries"`
}

type routeRuntimeEntry struct {
	APIHost       string          `json:"api_host"`
	NodeID        int             `json:"node_id"`
	ConfigVersion string          `json:"config_version"`
	NodeInfo      *panel.NodeInfo `json:"node_info"`
}

type routeRuntimeStateStore struct {
	dir string
}

func newRouteRuntimeStateStore(dir string) *routeRuntimeStateStore {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = conf.DefaultStatePath
	}
	return &routeRuntimeStateStore{dir: dir}
}

func (s *routeRuntimeStateStore) path() string {
	return filepath.Join(s.dir, "route-runtime.json")
}

func (s *routeRuntimeStateStore) Load() (*routeRuntimeState, error) {
	if s == nil {
		return nil, errors.New("route runtime state store is nil")
	}
	data, err := os.ReadFile(s.path())
	if err != nil {
		return nil, err
	}
	var state routeRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode route runtime state: %w", err)
	}
	if err := validateRouteRuntimeState(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *routeRuntimeStateStore) Save(state *routeRuntimeState) error {
	if s == nil {
		return errors.New("route runtime state store is nil")
	}
	if err := validateRouteRuntimeState(state); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create route runtime state directory: %w", err)
	}
	if err := os.Chmod(s.dir, 0700); err != nil {
		return fmt.Errorf("protect route runtime state directory: %w", err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode route runtime state: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".route-runtime-*")
	if err != nil {
		return fmt.Errorf("create route runtime state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect route runtime state temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write route runtime state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync route runtime state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close route runtime state temp file: %w", err)
	}
	if err := os.Rename(tmpPath, s.path()); err != nil {
		return fmt.Errorf("replace route runtime state: %w", err)
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(s.dir)
		if err != nil {
			return fmt.Errorf("open route runtime state directory: %w", err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("sync route runtime state directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close route runtime state directory: %w", closeErr)
		}
	}
	return nil
}

func validateRouteRuntimeState(state *routeRuntimeState) error {
	if state == nil {
		return errors.New("route runtime state is nil")
	}
	if state.Version != routeRuntimeStateVersion {
		return fmt.Errorf("unsupported route runtime state version %d", state.Version)
	}
	if state.SavedAt <= 0 || strings.TrimSpace(state.VectorHash) == "" || len(state.Entries) == 0 {
		return errors.New("route runtime state metadata is incomplete")
	}
	identities := make(map[string]struct{}, len(state.Entries))
	infos := make([]*panel.NodeInfo, 0, len(state.Entries))
	for _, entry := range state.Entries {
		host := normalizeAPIHost(entry.APIHost)
		if host == "" || host != entry.APIHost || entry.NodeID <= 0 || strings.TrimSpace(entry.ConfigVersion) == "" {
			return errors.New("route runtime state entry identity is incomplete")
		}
		if entry.NodeInfo == nil || entry.NodeInfo.Id != entry.NodeID || entry.NodeInfo.Common == nil {
			return errors.New("route runtime state entry NodeInfo is incomplete")
		}
		identity := routeRuntimeIdentity(host, entry.NodeID)
		if _, duplicate := identities[identity]; duplicate {
			return errors.New("route runtime state contains duplicate identities")
		}
		identities[identity] = struct{}{}
		infos = append(infos, entry.NodeInfo)
	}
	if _, err := core.CompileRouteRuntime(infos); err != nil {
		return fmt.Errorf("route runtime state failed strict validation: %w", err)
	}
	return nil
}

func routeRuntimeEntriesForConfigs(state *routeRuntimeState, configs []conf.NodeConfig) (map[string]routeRuntimeEntry, error) {
	if err := validateRouteRuntimeState(state); err != nil {
		return nil, err
	}
	return matchRouteRuntimeEntries(state, configs)
}

func matchRouteRuntimeEntries(state *routeRuntimeState, configs []conf.NodeConfig) (map[string]routeRuntimeEntry, error) {
	if len(state.Entries) != len(configs) {
		return nil, errors.New("route runtime state does not match configured node count")
	}
	entries := make(map[string]routeRuntimeEntry, len(state.Entries))
	for _, entry := range state.Entries {
		entries[routeRuntimeIdentity(entry.APIHost, entry.NodeID)] = entry
	}
	for _, config := range configs {
		identity := routeRuntimeIdentity(config.APIHost, config.NodeID)
		if _, ok := entries[identity]; !ok {
			return nil, errors.New("route runtime state identity does not match configured nodes")
		}
	}
	return entries, nil
}

func routeRuntimeIdentity(apiHost string, nodeID int) string {
	return fmt.Sprintf("%s|%d", normalizeAPIHost(apiHost), nodeID)
}

func overlayRouteRuntimeState(state *offlineState, entry routeRuntimeEntry) bool {
	if state == nil || entry.NodeInfo == nil || state.APIHost != normalizeAPIHost(entry.APIHost) ||
		state.NodeID != entry.NodeID || entry.NodeInfo.Id != entry.NodeID {
		return false
	}
	clone, err := cloneRouteRuntimeNodeInfo(entry.NodeInfo)
	if err != nil {
		return false
	}
	state.NodeInfo = clone
	return true
}

func cloneRouteRuntimeNodeInfo(info *panel.NodeInfo) (*panel.NodeInfo, error) {
	if info == nil {
		return nil, errors.New("NodeInfo is nil")
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	var clone panel.NodeInfo
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}
