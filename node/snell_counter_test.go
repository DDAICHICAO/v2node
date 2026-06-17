package node

import "testing"

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
