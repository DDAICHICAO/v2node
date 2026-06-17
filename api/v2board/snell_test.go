package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestGetManagedSnellDecodesDesiredState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/api/v2/server/managed-snell" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"generated_at":1760000000,"snell":[{"id":1,"name":"JP Snell","host":"jp.example.com","listen_ip":"::","version":6,"obfs":"http","obfs_host":"download.example.com","credentials":[{"user_id":1001,"port":20001,"psk":"secret","status":"active"}]}]}}`))
	}))
	defer server.Close()

	client := &Client{client: resty.New().SetBaseURL(server.URL)}

	state, err := client.GetManagedSnell(context.Background())
	if err != nil {
		t.Fatalf("GetManagedSnell: %v", err)
	}
	if state.GeneratedAt != 1760000000 {
		t.Fatalf("unexpected generated_at %d", state.GeneratedAt)
	}
	if len(state.Snell) != 1 {
		t.Fatalf("expected one snell node, got %d", len(state.Snell))
	}
	node := state.Snell[0]
	if node.ID != 1 || node.Name != "JP Snell" || node.Host != "jp.example.com" || node.ListenIP != "::" {
		t.Fatalf("unexpected snell node: %+v", node)
	}
	if node.Version != 6 || node.Obfs != "http" || node.ObfsHost != "download.example.com" {
		t.Fatalf("unexpected snell options: %+v", node)
	}
	if len(node.Credentials) != 1 {
		t.Fatalf("expected one credential, got %d", len(node.Credentials))
	}
	credential := node.Credentials[0]
	if credential.UserID != 1001 || credential.Port != 20001 || credential.PSK != "secret" || credential.Status != "active" {
		t.Fatalf("unexpected credential: %+v", credential)
	}
}

func TestReportManagedSnellStatusPostsPayload(t *testing.T) {
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/api/v2/server/managed-snell/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()

	client := &Client{client: resty.New().SetBaseURL(server.URL)}

	err := client.ReportManagedSnellStatus(context.Background(), map[string]string{"status": "running"})
	if err != nil {
		t.Fatalf("ReportManagedSnellStatus: %v", err)
	}
	if payload["status"] != "running" {
		t.Fatalf("unexpected status payload: %+v", payload)
	}
}

func TestReportManagedSnellTrafficPostsPayload(t *testing.T) {
	var payload struct {
		SnellID int                 `json:"snell_id"`
		Data    map[string][2]int64 `json:"data"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/api/v2/server/managed-snell/traffic" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()

	client := &Client{client: resty.New().SetBaseURL(server.URL)}

	err := client.ReportManagedSnellTraffic(context.Background(), 1, map[int][2]int64{1001: {123, 456}})
	if err != nil {
		t.Fatalf("ReportManagedSnellTraffic: %v", err)
	}
	if payload.SnellID != 1 {
		t.Fatalf("unexpected snell_id %d", payload.SnellID)
	}
	if payload.Data["1001"] != [2]int64{123, 456} {
		t.Fatalf("unexpected traffic payload: %+v", payload.Data)
	}
}
