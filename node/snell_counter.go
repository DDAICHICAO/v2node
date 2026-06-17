package node

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type snellPortCounters struct {
	Upload   int64
	Download int64
}

type snellCounterDelta struct {
	Upload   int64
	Download int64
}

type snellCounterState struct {
	last map[int]snellPortCounters
}

type snellCounterReader interface {
	ReadPorts([]int) (map[int]snellPortCounters, error)
}

type commandRunner func(name string, args ...string) ([]byte, error)

type snellCommandCounterReader struct {
	run commandRunner
}

func newSnellCounterState() *snellCounterState {
	return &snellCounterState{last: make(map[int]snellPortCounters)}
}

func (s *snellCounterState) Delta(port int, current snellPortCounters) snellCounterDelta {
	if s.last == nil {
		s.last = make(map[int]snellPortCounters)
	}
	previous, ok := s.last[port]
	s.last[port] = current
	if !ok {
		return snellCounterDelta{}
	}
	if current.Upload < previous.Upload || current.Download < previous.Download {
		return snellCounterDelta{}
	}
	return snellCounterDelta{
		Upload:   current.Upload - previous.Upload,
		Download: current.Download - previous.Download,
	}
}

func newDefaultSnellCounterReader() snellCounterReader {
	return snellCommandCounterReader{run: defaultCommandRunner}
}

func defaultCommandRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func (r snellCommandCounterReader) ReadPorts(ports []int) (map[int]snellPortCounters, error) {
	if len(ports) == 0 {
		return map[int]snellPortCounters{}, nil
	}
	run := r.run
	if run == nil {
		run = defaultCommandRunner
	}

	nftOutput, nftErr := run("nft", "-a", "list", "ruleset")
	if nftErr == nil {
		return parseNftSnellCounters(nftOutput, ports), nil
	}

	iptablesOutput, iptablesErr := run("iptables", "-vnx", "-L")
	if iptablesErr != nil {
		return nil, fmt.Errorf("read snell counters: nftables failed: %v; iptables failed: %w", nftErr, iptablesErr)
	}
	natOutput, _ := run("iptables", "-t", "nat", "-vnx", "-L")
	combined := append(append([]byte{}, iptablesOutput...), '\n')
	combined = append(combined, natOutput...)
	return parseIPTablesSnellCounters(combined, ports), nil
}

func parseNftSnellCounters(output []byte, ports []int) map[int]snellPortCounters {
	counters := zeroSnellCounters(ports)
	portSet := snellPortSet(ports)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		bytesValue, ok := nftLineBytes(line)
		if !ok {
			continue
		}
		for port := range portSet {
			portText := strconv.Itoa(port)
			current := counters[port]
			if strings.Contains(line, "dport "+portText) {
				current.Upload += bytesValue
			}
			if strings.Contains(line, "sport "+portText) {
				current.Download += bytesValue
			}
			counters[port] = current
		}
	}
	return counters
}

func parseIPTablesSnellCounters(output []byte, ports []int) map[int]snellPortCounters {
	counters := zeroSnellCounters(ports)
	portSet := snellPortSet(ports)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		bytesValue, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		for port := range portSet {
			portText := strconv.Itoa(port)
			current := counters[port]
			if strings.Contains(line, "dpt:"+portText) {
				current.Upload += bytesValue
			}
			if strings.Contains(line, "spt:"+portText) {
				current.Download += bytesValue
			}
			counters[port] = current
		}
	}
	return counters
}

func nftLineBytes(line string) (int64, bool) {
	fields := strings.Fields(line)
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] != "bytes" {
			continue
		}
		value, err := strconv.ParseInt(fields[i+1], 10, 64)
		return value, err == nil
	}
	return 0, false
}

func zeroSnellCounters(ports []int) map[int]snellPortCounters {
	counters := make(map[int]snellPortCounters, len(ports))
	for _, port := range ports {
		counters[port] = snellPortCounters{}
	}
	return counters
}

func snellPortSet(ports []int) map[int]struct{} {
	set := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		set[port] = struct{}{}
	}
	return set
}
