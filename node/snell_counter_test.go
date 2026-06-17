package node

import (
	"strings"
	"testing"
)

func TestSnellCounterDeltaReportsPositiveDifference(t *testing.T) {
	state := newSnellCounterState()

	first := state.Delta(20001, snellPortCounters{Upload: 100, Download: 200})
	if first.Upload != 0 || first.Download != 0 {
		t.Fatalf("first sample delta=%+v, want zero", first)
	}

	second := state.Delta(20001, snellPortCounters{Upload: 150, Download: 275})
	if second.Upload != 50 || second.Download != 75 {
		t.Fatalf("second sample delta=%+v, want upload=50 download=75", second)
	}
}

func TestSnellCounterDeltaSkipsCounterReset(t *testing.T) {
	state := newSnellCounterState()

	_ = state.Delta(20001, snellPortCounters{Upload: 100, Download: 200})
	reset := state.Delta(20001, snellPortCounters{Upload: 40, Download: 90})
	if reset.Upload != 0 || reset.Download != 0 {
		t.Fatalf("reset delta=%+v, want zero", reset)
	}

	next := state.Delta(20001, snellPortCounters{Upload: 45, Download: 110})
	if next.Upload != 5 || next.Download != 20 {
		t.Fatalf("post-reset delta=%+v, want upload=5 download=20", next)
	}
}

func TestSnellCounterDeltaKeepsPositiveDirectionWhenOtherDirectionResets(t *testing.T) {
	state := newSnellCounterState()

	_ = state.Delta(20001, snellPortCounters{Upload: 100, Download: 200})
	next := state.Delta(20001, snellPortCounters{Upload: 150, Download: 20})
	if next.Upload != 50 || next.Download != 0 {
		t.Fatalf("partial reset delta=%+v, want upload=50 download=0", next)
	}
}

func TestSnellCounterPendingDeltaDoesNotAdvanceBaselineUntilCommit(t *testing.T) {
	state := newSnellCounterState()
	state.last[20001] = snellPortCounters{Upload: 20, Download: 80}

	pending := state.PendingDelta(20001, snellPortCounters{Upload: 100, Download: 200})
	if pending.Upload != 80 || pending.Download != 120 {
		t.Fatalf("pending delta=%+v, want upload=80 download=120", pending)
	}
	if got := state.last[20001]; got != (snellPortCounters{Upload: 20, Download: 80}) {
		t.Fatalf("pending advanced baseline: %+v", got)
	}

	state.Commit(20001, snellPortCounters{Upload: 100, Download: 200})
	if got := state.last[20001]; got != (snellPortCounters{Upload: 100, Download: 200}) {
		t.Fatalf("commit baseline=%+v, want 100/200", got)
	}
}

func TestSnellCounterForgetClearsPortBaseline(t *testing.T) {
	state := newSnellCounterState()

	_ = state.Delta(20001, snellPortCounters{Upload: 100, Download: 200})
	state.Forget(20001)

	next := state.Delta(20001, snellPortCounters{Upload: 150, Download: 275})
	if next.Upload != 0 || next.Download != 0 {
		t.Fatalf("after forget delta=%+v, want zero", next)
	}
}

func TestParseNftSnellCountersMatchesExactPort(t *testing.T) {
	output := []byte(`
table inet filter {
 chain input {
  tcp dport 20001 counter packets 1 bytes 100
  tcp dport 200010 counter packets 1 bytes 999
  tcp sport 20001 counter packets 1 bytes 200
  tcp sport 120001 counter packets 1 bytes 888
 }
}`)

	counters := parseNftSnellCounters(output, []int{20001})
	got := counters[20001]
	if got.Upload != 100 || got.Download != 200 {
		t.Fatalf("nft counters=%+v, want upload=100 download=200", got)
	}
}

func TestParseIPTablesSnellCountersMatchesExactPort(t *testing.T) {
	output := []byte(`
pkts bytes target prot opt in out source destination tcp dpt:20001
1 999 ACCEPT tcp -- * * 0.0.0.0/0 0.0.0.0/0 tcp dpt:200010
1 100 ACCEPT tcp -- * * 0.0.0.0/0 0.0.0.0/0 tcp dpt:20001
1 200 ACCEPT tcp -- * * 0.0.0.0/0 0.0.0.0/0 tcp spt:20001
1 888 ACCEPT tcp -- * * 0.0.0.0/0 0.0.0.0/0 tcp spt:120001
`)

	counters := parseIPTablesSnellCounters(output, []int{20001})
	got := counters[20001]
	if got.Upload != 100 || got.Download != 200 {
		t.Fatalf("iptables counters=%+v, want upload=100 download=200", got)
	}
}

func TestSnellCommandCounterReaderEnsuresManagedNftRules(t *testing.T) {
	var calls []string
	run := func(name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "nft list chain inet v2node_snell input", "nft list chain inet v2node_snell output":
			return []byte{}, nil
		case "nft list table inet v2node_snell":
			return []byte(`
table inet v2node_snell {
 chain input {
  tcp dport 20001 counter packets 1 bytes 100 comment "v2node-snell-upload"
  tcp dport 20001 counter packets 1 bytes 999
 }
 chain output {
  tcp sport 20001 counter packets 1 bytes 200 comment "v2node-snell-download"
  tcp sport 20001 counter packets 1 bytes 888
 }
}`), nil
		default:
			return []byte{}, nil
		}
	}

	counters, err := (snellCommandCounterReader{run: run}).ReadPorts([]int{20001})
	if err != nil {
		t.Fatalf("ReadPorts: %v", err)
	}
	if got := counters[20001]; got != (snellPortCounters{Upload: 100, Download: 200}) {
		t.Fatalf("counters=%+v, want upload=100 download=200", got)
	}

	wantRule := "nft add rule inet v2node_snell input tcp dport 20001 counter comment v2node-snell-upload"
	if !containsString(calls, wantRule) {
		t.Fatalf("expected managed nft rule %q in calls: %+v", wantRule, calls)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
