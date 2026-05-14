package netstat

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Snapshot struct {
	Interfaces []string
	RxBytes    uint64
	TxBytes    uint64
	CapturedAt time.Time
}

type Throughput struct {
	Interfaces      []string
	RxBytes         uint64
	TxBytes         uint64
	RxBps           int64
	TxBps           int64
	IntervalSeconds float64
	CapturedAt      time.Time
}

type Sampler struct {
	mu       sync.Mutex
	previous *Snapshot
}

func NewSampler() *Sampler {
	return &Sampler{}
}

func (s *Sampler) Sample() (*Throughput, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := readSnapshot()
	if err != nil {
		return nil, false, err
	}
	if s.previous == nil {
		s.previous = current
		return nil, false, nil
	}

	interval := current.CapturedAt.Sub(s.previous.CapturedAt).Seconds()
	if interval <= 0 {
		s.previous = current
		return nil, false, nil
	}

	rxDelta := safeDelta(current.RxBytes, s.previous.RxBytes)
	txDelta := safeDelta(current.TxBytes, s.previous.TxBytes)
	throughput := &Throughput{
		Interfaces:      current.Interfaces,
		RxBytes:         current.RxBytes,
		TxBytes:         current.TxBytes,
		RxBps:           int64(float64(rxDelta*8) / interval),
		TxBps:           int64(float64(txDelta*8) / interval),
		IntervalSeconds: interval,
		CapturedAt:      current.CapturedAt,
	}
	s.previous = current
	return throughput, true, nil
}

func readSnapshot() (*Snapshot, error) {
	stats, err := readProcNetDev()
	if err != nil {
		return nil, err
	}

	selected := defaultRouteInterfaces()
	rx, tx, ifaces := sumStats(stats, selected)
	if len(ifaces) == 0 {
		selected = activeInterfaces()
		rx, tx, ifaces = sumStats(stats, selected)
	}
	if len(ifaces) == 0 {
		return nil, fmt.Errorf("no active network interface statistics found")
	}

	return &Snapshot{
		Interfaces: ifaces,
		RxBytes:    rx,
		TxBytes:    tx,
		CapturedAt: time.Now(),
	}, nil
}

type interfaceStats struct {
	rxBytes uint64
	txBytes uint64
}

func readProcNetDev() (map[string]interfaceStats, error) {
	bytes, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}

	stats := make(map[string]interfaceStats)
	lines := strings.Split(string(bytes), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if name == "" || len(fields) < 16 {
			continue
		}
		rx, errRx := strconv.ParseUint(fields[0], 10, 64)
		tx, errTx := strconv.ParseUint(fields[8], 10, 64)
		if errRx != nil || errTx != nil {
			continue
		}
		stats[name] = interfaceStats{rxBytes: rx, txBytes: tx}
	}
	return stats, nil
}

func defaultRouteInterfaces() []string {
	bytes, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil
	}

	seen := make(map[string]struct{})
	for _, line := range strings.Split(string(bytes), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] == "Iface" || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 64)
		if err != nil || flags&0x1 == 0 {
			continue
		}
		seen[fields[0]] = struct{}{}
	}
	return sortedKeys(seen)
}

func activeInterfaces() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if ignoredInterface(iface.Name) {
			continue
		}
		seen[iface.Name] = struct{}{}
	}
	return sortedKeys(seen)
}

func sumStats(stats map[string]interfaceStats, selected []string) (uint64, uint64, []string) {
	var rx uint64
	var tx uint64
	var used []string
	for _, name := range selected {
		item, ok := stats[name]
		if !ok {
			continue
		}
		rx += item.rxBytes
		tx += item.txBytes
		used = append(used, name)
	}
	sort.Strings(used)
	return rx, tx, used
}

func ignoredInterface(name string) bool {
	name = strings.ToLower(name)
	if name == "lo" {
		return true
	}
	prefixes := []string{
		"br-", "cali", "cni", "docker", "dummy", "erspan", "flannel",
		"gre", "gretap", "ifb", "ip6tnl", "kube", "sit", "veth", "virbr",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func safeDelta(current, previous uint64) uint64 {
	if current < previous {
		return 0
	}
	return current - previous
}
