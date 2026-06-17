package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

const defaultManagedSnellConfigDir = "/etc/v2node/snell"

type managedSnellAPI interface {
	GetManagedSnell(context.Context) (*panel.ManagedSnellState, error)
	ReportManagedSnellStatus(context.Context, any) error
	ReportManagedSnellTraffic(context.Context, int, []panel.ManagedSnellTrafficUser) error
}

type managedSnellTrafficReporter interface {
	ReportManagedSnellTraffic(context.Context, int, []panel.ManagedSnellTrafficUser) error
}

type managedSnellListener struct {
	Snell      panel.ManagedSnellNode
	Credential panel.ManagedSnellCredential
}

type managedSnellStatus struct {
	SnellID int    `json:"snell_id"`
	UserID  int    `json:"user_id"`
	Port    int    `json:"port"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type managedSnellStatusesRequest struct {
	Statuses []managedSnellStatus `json:"statuses"`
}

type managedSnellManager struct {
	mu            sync.Mutex
	configDir     string
	binary        string
	counterReader snellCounterReader
	counterState  *snellCounterState
	processes     map[string]*snellProcess
	listeners     map[string]managedSnellListener
	closed        bool
	writeConfig   func(string, panel.ManagedSnellNode, panel.ManagedSnellCredential) (string, error)
	startProcess  func(string, string) (*exec.Cmd, error)
	stopProcess   func(*exec.Cmd) error
	removeFile    func(string) error
}

func newManagedSnellManager(_ *panel.Client) *managedSnellManager {
	return &managedSnellManager{
		configDir:     defaultManagedSnellConfigDir,
		counterReader: newDefaultSnellCounterReader(),
		counterState:  newSnellCounterState(),
		processes:     make(map[string]*snellProcess),
		listeners:     make(map[string]managedSnellListener),
		writeConfig:   writeSnellConfig,
		startProcess:  startSnellProcess,
		stopProcess:   stopSnellProcess,
		removeFile:    os.Remove,
	}
}

func managedSnellDesiredListeners(state *panel.ManagedSnellState) map[string]managedSnellListener {
	listeners := make(map[string]managedSnellListener)
	if state == nil {
		return listeners
	}
	for _, snellNode := range state.Snell {
		listenerNode := snellNode
		listenerNode.Credentials = nil
		for _, credential := range snellNode.Credentials {
			if !managedSnellCredentialActive(credential) {
				continue
			}
			key := snellListenerKey(listenerNode.ID, credential.UserID, credential.Port)
			listeners[key] = managedSnellListener{
				Snell:      listenerNode,
				Credential: credential,
			}
		}
	}
	return listeners
}

func managedSnellCredentialActive(credential panel.ManagedSnellCredential) bool {
	return strings.EqualFold(strings.TrimSpace(credential.Status), "active") &&
		credential.UserID > 0 &&
		credential.Port > 0 &&
		strings.TrimSpace(credential.PSK) != ""
}

func (m *managedSnellManager) syncDesiredState(ctx context.Context, api managedSnellAPI) error {
	if m == nil || api == nil {
		return nil
	}
	state, err := api.GetManagedSnell(ctx)
	if err != nil {
		return err
	}
	statuses := m.reconcile(managedSnellDesiredListeners(state))
	if err := api.ReportManagedSnellStatus(ctx, managedSnellStatusesRequest{Statuses: statuses}); err != nil {
		return err
	}
	return nil
}

func (m *managedSnellManager) reconcile(desired map[string]managedSnellListener) []managedSnellStatus {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.processes == nil {
		m.processes = make(map[string]*snellProcess)
	}
	if m.listeners == nil {
		m.listeners = make(map[string]managedSnellListener)
	}
	if m.closed {
		return nil
	}

	var statuses []managedSnellStatus
	for key, process := range m.processes {
		listener, wanted := desired[key]
		current := m.listeners[key]
		if wanted && reflect.DeepEqual(current, listener) {
			if exited, message := processExited(process); exited {
				statuses = append(statuses, m.stopLocked(key, process, "exited")...)
				if message != "" && len(statuses) > 0 {
					statuses[len(statuses)-1].Message = message
				}
				continue
			}
			statuses = append(statuses, managedSnellStatus{
				SnellID: process.SnellID,
				UserID:  process.UserID,
				Port:    process.Port,
				Status:  "running",
			})
			continue
		}
		statuses = append(statuses, m.stopLocked(key, process, "stopped")...)
	}

	keys := make([]string, 0, len(desired))
	for key := range desired {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := m.processes[key]; exists {
			continue
		}
		listener := desired[key]
		statuses = append(statuses, m.startLocked(key, listener))
	}
	return statuses
}

func (m *managedSnellManager) startLocked(key string, listener managedSnellListener) managedSnellStatus {
	status := managedSnellStatus{
		SnellID: listener.Snell.ID,
		UserID:  listener.Credential.UserID,
		Port:    listener.Credential.Port,
		Status:  "running",
	}
	configPath, err := m.writeConfig(m.configDir, listener.Snell, listener.Credential)
	if err != nil {
		status.Status = "error"
		status.Message = err.Error()
		return status
	}
	cmd, err := m.startProcess(m.binary, configPath)
	if err != nil {
		_ = m.removeFile(configPath)
		status.Status = "error"
		status.Message = err.Error()
		return status
	}
	m.processes[key] = &snellProcess{
		Key:        key,
		SnellID:    listener.Snell.ID,
		UserID:     listener.Credential.UserID,
		Port:       listener.Credential.Port,
		ConfigPath: configPath,
		Command:    cmd,
		Done:       watchSnellProcess(cmd),
	}
	m.listeners[key] = listener
	return status
}

func (m *managedSnellManager) stopLocked(key string, process *snellProcess, status string) []managedSnellStatus {
	if process == nil {
		delete(m.processes, key)
		delete(m.listeners, key)
		return nil
	}
	result := managedSnellStatus{
		SnellID: process.SnellID,
		UserID:  process.UserID,
		Port:    process.Port,
		Status:  status,
	}
	if err := m.stopProcess(process.Command); err != nil {
		result.Status = "error"
		result.Message = err.Error()
	}
	if process.Done != nil {
		select {
		case <-process.Done:
		case <-time.After(2 * time.Second):
			if result.Message == "" {
				result.Status = "error"
				result.Message = "timeout waiting snell process exit"
			}
		}
	}
	if strings.TrimSpace(process.ConfigPath) != "" {
		if err := m.removeFile(process.ConfigPath); err != nil && !os.IsNotExist(err) && result.Message == "" {
			result.Status = "error"
			result.Message = err.Error()
		}
	}
	if m.counterState != nil {
		m.counterState.Forget(process.Port)
	}
	delete(m.processes, key)
	delete(m.listeners, key)
	return []managedSnellStatus{result}
}

func (m *managedSnellManager) reportTraffic(ctx context.Context, reporter managedSnellTrafficReporter) error {
	if m == nil || reporter == nil {
		return nil
	}
	processes := m.snapshotProcesses()
	if len(processes) == 0 {
		return nil
	}
	ports := make([]int, 0, len(processes))
	for _, process := range processes {
		if process != nil && process.Port > 0 {
			ports = append(ports, process.Port)
		}
	}
	sort.Ints(ports)
	reader := m.counterReader
	if reader == nil {
		reader = newDefaultSnellCounterReader()
	}
	counters, err := reader.ReadPorts(ports)
	if err != nil {
		return err
	}
	processes, deltas, currentCounters := m.trafficDeltas(counters)
	batches := buildSnellTrafficBatches(processes, deltas, 1)
	snellIDs := make([]int, 0, len(batches))
	for snellID := range batches {
		snellIDs = append(snellIDs, snellID)
	}
	sort.Ints(snellIDs)
	for _, snellID := range snellIDs {
		for _, row := range batches[snellID] {
			rows := []panel.ManagedSnellTrafficUser{row}
			if err := reporter.ReportManagedSnellTraffic(ctx, snellID, rows); err != nil {
				return err
			}
			m.commitTrafficCounters(rows, currentCounters)
		}
	}
	return nil
}

func (m *managedSnellManager) snapshotProcesses() map[string]*snellProcess {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	copied := make(map[string]*snellProcess, len(m.processes))
	for key, process := range m.processes {
		if process == nil {
			continue
		}
		cloned := *process
		copied[key] = &cloned
	}
	return copied
}

func (m *managedSnellManager) trafficDeltas(counters map[int]snellPortCounters) (map[string]*snellProcess, map[int]snellCounterDelta, map[int]snellPortCounters) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.counterState == nil {
		m.counterState = newSnellCounterState()
	}
	processes := make(map[string]*snellProcess, len(m.processes))
	deltas := make(map[int]snellCounterDelta, len(counters))
	currentCounters := make(map[int]snellPortCounters, len(counters))
	for key, process := range m.processes {
		if process == nil {
			continue
		}
		current, ok := counters[process.Port]
		if !ok {
			continue
		}
		cloned := *process
		processes[key] = &cloned
		currentCounters[process.Port] = current
		deltas[process.Port] = m.counterState.PendingDelta(process.Port, current)
	}
	return processes, deltas, currentCounters
}

func buildSnellTrafficBatches(processes map[string]*snellProcess, deltas map[int]snellCounterDelta, minBytes int64) map[int][]panel.ManagedSnellTrafficUser {
	result := make(map[int][]panel.ManagedSnellTrafficUser)
	keys := make([]string, 0, len(processes))
	for key := range processes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		process := processes[key]
		if process == nil || process.SnellID <= 0 || process.UserID <= 0 || process.Port <= 0 {
			continue
		}
		delta := deltas[process.Port]
		if delta.Upload+delta.Download < minBytes {
			continue
		}
		result[process.SnellID] = append(result[process.SnellID], panel.ManagedSnellTrafficUser{
			UserID:   process.UserID,
			Port:     process.Port,
			Upload:   delta.Upload,
			Download: delta.Download,
		})
	}
	return result
}

func (m *managedSnellManager) commitTrafficCounters(rows []panel.ManagedSnellTrafficUser, counters map[int]snellPortCounters) {
	if m == nil || m.counterState == nil {
		return
	}
	for _, row := range rows {
		current, ok := counters[row.Port]
		if !ok {
			continue
		}
		m.counterState.Commit(row.Port, current)
	}
}

func (m *managedSnellManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	var firstErr error
	for key, process := range m.processes {
		for _, status := range m.stopLocked(key, process, "stopped") {
			if status.Status == "error" && firstErr == nil {
				firstErr = fmt.Errorf("stop snell listener %d/%d:%d: %s", status.SnellID, status.UserID, status.Port, status.Message)
			}
		}
	}
	m.processes = make(map[string]*snellProcess)
	m.listeners = make(map[string]managedSnellListener)
	return firstErr
}
