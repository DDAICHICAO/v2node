package node

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

type fakeSnellCounterReader struct {
	counters map[int]snellPortCounters
}

func (r fakeSnellCounterReader) ReadPorts(ports []int) (map[int]snellPortCounters, error) {
	result := make(map[int]snellPortCounters, len(ports))
	for _, port := range ports {
		result[port] = r.counters[port]
	}
	return result, nil
}

type recordingSnellReporter struct {
	traffic map[int][]panel.ManagedSnellTrafficUser
	err     error
}

func (r *recordingSnellReporter) ReportManagedSnellTraffic(_ context.Context, snellID int, data []panel.ManagedSnellTrafficUser) error {
	if r.err != nil {
		return r.err
	}
	if r.traffic == nil {
		r.traffic = make(map[int][]panel.ManagedSnellTrafficUser)
	}
	copied := append([]panel.ManagedSnellTrafficUser(nil), data...)
	r.traffic[snellID] = append(r.traffic[snellID], copied...)
	return nil
}

func TestManagedSnellDesiredStateBuildsActiveListeners(t *testing.T) {
	state := &panel.ManagedSnellState{
		Snell: []panel.ManagedSnellNode{
			{
				ID:       1,
				ListenIP: "::",
				Version:  6,
				Credentials: []panel.ManagedSnellCredential{
					{UserID: 1001, Port: 20001, PSK: "secret-a", Status: "active"},
					{UserID: 1002, Port: 20002, PSK: "secret-b", Status: "active"},
				},
			},
		},
	}

	listeners := managedSnellDesiredListeners(state)

	if len(listeners) != 2 {
		t.Fatalf("expected two listeners, got %d", len(listeners))
	}
	first := listeners[snellListenerKey(1, 1001, 20001)]
	if first.Snell.ID != 1 || first.Credential.UserID != 1001 || first.Credential.Port != 20001 || first.Credential.PSK != "secret-a" {
		t.Fatalf("unexpected first listener: %+v", first)
	}
	second := listeners[snellListenerKey(1, 1002, 20002)]
	if second.Snell.ID != 1 || second.Credential.UserID != 1002 || second.Credential.Port != 20002 || second.Credential.PSK != "secret-b" {
		t.Fatalf("unexpected second listener: %+v", second)
	}
}

func TestManagedSnellDesiredStateSkipsDisabledCredentials(t *testing.T) {
	state := &panel.ManagedSnellState{
		Snell: []panel.ManagedSnellNode{
			{
				ID: 1,
				Credentials: []panel.ManagedSnellCredential{
					{UserID: 1001, Port: 20001, PSK: "active", Status: "active"},
					{UserID: 1002, Port: 20002, PSK: "disabled", Status: "disabled"},
					{UserID: 1003, Port: 20003, PSK: "blank"},
				},
			},
		},
	}

	listeners := managedSnellDesiredListeners(state)

	if len(listeners) != 1 {
		t.Fatalf("expected only active listener, got %d: %+v", len(listeners), listeners)
	}
	if _, ok := listeners[snellListenerKey(1, 1001, 20001)]; !ok {
		t.Fatalf("expected active credential listener, got %+v", listeners)
	}
}

func TestManagedSnellTrafficBatchAggregatesBySnellAndUser(t *testing.T) {
	manager := newManagedSnellManager(nil)
	manager.counterReader = fakeSnellCounterReader{counters: map[int]snellPortCounters{
		20001: {Upload: 100, Download: 200},
		20002: {Upload: 50, Download: 75},
		20003: {Upload: 400, Download: 800},
	}}
	manager.counterState.last = map[int]snellPortCounters{
		20001: {Upload: 20, Download: 80},
		20002: {Upload: 10, Download: 15},
		20003: {Upload: 300, Download: 600},
	}
	manager.processes = map[string]*snellProcess{
		snellListenerKey(1, 1001, 20001): {SnellID: 1, UserID: 1001, Port: 20001},
		snellListenerKey(1, 1001, 20002): {SnellID: 1, UserID: 1001, Port: 20002},
		snellListenerKey(2, 2001, 20003): {SnellID: 2, UserID: 2001, Port: 20003},
	}
	reporter := &recordingSnellReporter{}

	if err := manager.reportTraffic(context.Background(), reporter); err != nil {
		t.Fatalf("reportTraffic: %v", err)
	}

	want := map[int][]panel.ManagedSnellTrafficUser{
		1: {
			{UserID: 1001, Port: 20001, Upload: 80, Download: 120},
			{UserID: 1001, Port: 20002, Upload: 40, Download: 60},
		},
		2: {
			{UserID: 2001, Port: 20003, Upload: 100, Download: 200},
		},
	}
	if !reflect.DeepEqual(reporter.traffic, want) {
		t.Fatalf("traffic=%+v, want %+v", reporter.traffic, want)
	}
}

func TestManagedSnellTrafficKeepsBaselineWhenReportFails(t *testing.T) {
	manager := newManagedSnellManager(nil)
	manager.counterReader = fakeSnellCounterReader{counters: map[int]snellPortCounters{
		20001: {Upload: 100, Download: 200},
	}}
	manager.counterState.last = map[int]snellPortCounters{
		20001: {Upload: 20, Download: 80},
	}
	manager.processes = map[string]*snellProcess{
		snellListenerKey(1, 1001, 20001): {SnellID: 1, UserID: 1001, Port: 20001},
	}

	err := manager.reportTraffic(context.Background(), &recordingSnellReporter{err: fmt.Errorf("temporary error")})
	if err == nil {
		t.Fatal("expected report error")
	}
	if got := manager.counterState.last[20001]; got != (snellPortCounters{Upload: 20, Download: 80}) {
		t.Fatalf("baseline advanced after failed report: %+v", got)
	}

	reporter := &recordingSnellReporter{}
	if err := manager.reportTraffic(context.Background(), reporter); err != nil {
		t.Fatalf("retry reportTraffic: %v", err)
	}
	want := []panel.ManagedSnellTrafficUser{
		{UserID: 1001, Port: 20001, Upload: 80, Download: 120},
	}
	if !reflect.DeepEqual(reporter.traffic[1], want) {
		t.Fatalf("retry traffic=%+v, want %+v", reporter.traffic[1], want)
	}
	if got := manager.counterState.last[20001]; got != (snellPortCounters{Upload: 100, Download: 200}) {
		t.Fatalf("baseline not advanced after successful retry: %+v", got)
	}
}
