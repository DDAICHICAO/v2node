package node

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	mu   sync.Mutex
	last map[int]snellPortCounters
}

type snellCounterReader interface {
	ReadPorts([]int) (map[int]snellPortCounters, error)
}

type commandRunner func(name string, args ...string) ([]byte, error)

type snellCommandCounterReader struct {
	run commandRunner
}

const snellNftTable = "v2node_snell"
const snellNftUploadComment = "v2node-snell-upload"
const snellNftDownloadComment = "v2node-snell-download"

func newSnellCounterState() *snellCounterState {
	return &snellCounterState{last: make(map[int]snellPortCounters)}
}

func (s *snellCounterState) Delta(port int, current snellPortCounters) snellCounterDelta {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = make(map[int]snellPortCounters)
	}
	previous, ok := s.last[port]
	s.last[port] = current
	if !ok {
		return snellCounterDelta{}
	}
	delta := snellCounterDelta{}
	if current.Upload >= previous.Upload {
		delta.Upload = current.Upload - previous.Upload
	}
	if current.Download >= previous.Download {
		delta.Download = current.Download - previous.Download
	}
	return delta
}

func (s *snellCounterState) PendingDelta(port int, current snellPortCounters) snellCounterDelta {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = make(map[int]snellPortCounters)
	}
	previous, ok := s.last[port]
	if !ok {
		s.last[port] = current
		return snellCounterDelta{}
	}
	delta := snellCounterDelta{}
	if current.Upload >= previous.Upload {
		delta.Upload = current.Upload - previous.Upload
	}
	if current.Download >= previous.Download {
		delta.Download = current.Download - previous.Download
	}
	return delta
}

func (s *snellCounterState) Commit(port int, current snellPortCounters) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		s.last = make(map[int]snellPortCounters)
	}
	s.last[port] = current
}

func (s *snellCounterState) Forget(port int) {
	if s == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.last == nil {
		return
	}
	delete(s.last, port)
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

	if ensureErr := ensureNftSnellCounters(run, ports); ensureErr != nil {
		return nil, fmt.Errorf("ensure snell nft counters: %w", ensureErr)
	}
	nftOutput, nftErr := run("nft", "list", "table", "inet", snellNftTable)
	if nftErr != nil {
		return nil, fmt.Errorf("read snell nft counters: %w", nftErr)
	}
	return parseManagedNftSnellCounters(nftOutput, ports), nil
}

func ensureNftSnellCounters(run commandRunner, ports []int) error {
	if len(ports) == 0 {
		return nil
	}
	if err := nftIgnoreExists(run("nft", "add", "table", "inet", snellNftTable)); err != nil {
		return err
	}
	if err := nftIgnoreExists(run("nft", "add", "chain", "inet", snellNftTable, "input", "{", "type", "filter", "hook", "input", "priority", "0", ";", "policy", "accept", ";", "}")); err != nil {
		return err
	}
	if err := nftIgnoreExists(run("nft", "add", "chain", "inet", snellNftTable, "output", "{", "type", "filter", "hook", "output", "priority", "0", ";", "policy", "accept", ";", "}")); err != nil {
		return err
	}

	inputOutput, inputErr := run("nft", "list", "chain", "inet", snellNftTable, "input")
	if inputErr != nil {
		return inputErr
	}
	outputOutput, outputErr := run("nft", "list", "chain", "inet", snellNftTable, "output")
	if outputErr != nil {
		return outputErr
	}

	for _, port := range ports {
		portText := strconv.Itoa(port)
		if !nftManagedRuleExists(inputOutput, "dport", port, snellNftUploadComment) {
			if err := nftIgnoreExists(run("nft", "add", "rule", "inet", snellNftTable, "input", "tcp", "dport", portText, "counter", "comment", snellNftUploadComment)); err != nil {
				return err
			}
		}
		if !nftManagedRuleExists(outputOutput, "sport", port, snellNftDownloadComment) {
			if err := nftIgnoreExists(run("nft", "add", "rule", "inet", snellNftTable, "output", "tcp", "sport", portText, "counter", "comment", snellNftDownloadComment)); err != nil {
				return err
			}
		}
	}
	return nil
}

func nftIgnoreExists(output []byte, err error) error {
	if err == nil {
		return nil
	}
	if bytes.Contains(bytes.ToLower(output), []byte("exists")) {
		return nil
	}
	return err
}

func nftManagedRuleExists(output []byte, field string, port int, comment string) bool {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "tcp") &&
			strings.Contains(line, "counter") &&
			strings.Contains(line, comment) &&
			nftLineHasPort(line, field, port) {
			return true
		}
	}
	return false
}

func parseManagedNftSnellCounters(output []byte, ports []int) map[int]snellPortCounters {
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
			current := counters[port]
			if strings.Contains(line, snellNftUploadComment) && nftLineHasPort(line, "dport", port) {
				current.Upload += bytesValue
			}
			if strings.Contains(line, snellNftDownloadComment) && nftLineHasPort(line, "sport", port) {
				current.Download += bytesValue
			}
			counters[port] = current
		}
	}
	return counters
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
			current := counters[port]
			if nftLineHasPort(line, "dport", port) {
				current.Upload += bytesValue
			}
			if nftLineHasPort(line, "sport", port) {
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
		bytesValue, ok := parseSnellCounterInt(fields[1])
		if !ok {
			continue
		}
		for port := range portSet {
			current := counters[port]
			if iptablesLineHasPort(line, "dpt:", port) {
				current.Upload += bytesValue
			}
			if iptablesLineHasPort(line, "spt:", port) {
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
		value, ok := parseSnellCounterInt(fields[i+1])
		return value, ok
	}
	return 0, false
}

func nftLineHasPort(line, field string, port int) bool {
	normalized := strings.NewReplacer(",", " ", "{", " ", "}", " ").Replace(line)
	fields := strings.Fields(normalized)
	for i, token := range fields {
		if token != field {
			continue
		}
		for j := i + 1; j < len(fields); j++ {
			switch fields[j] {
			case "counter", "packets", "bytes":
				return false
			}
			if tokenPortEquals(fields[j], port) {
				return true
			}
		}
	}
	return false
}

func iptablesLineHasPort(line, prefix string, port int) bool {
	for _, field := range strings.Fields(line) {
		if !strings.HasPrefix(field, prefix) {
			continue
		}
		if tokenPortEquals(strings.TrimPrefix(field, prefix), port) {
			return true
		}
	}
	return false
}

func tokenPortEquals(token string, port int) bool {
	value, err := strconv.Atoi(strings.Trim(token, " ,"))
	return err == nil && value == port
}

func parseSnellCounterInt(value string) (int64, bool) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err == nil {
		return parsed, true
	}
	unsigned, unsignedErr := strconv.ParseUint(value, 10, 64)
	if unsignedErr != nil {
		return 0, false
	}
	const maxInt64 = uint64(1<<63 - 1)
	if unsigned > maxInt64 {
		return int64(maxInt64), true
	}
	return int64(unsigned), true
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
