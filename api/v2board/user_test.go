package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestUserSyncSeqKeepsHighestSequence(t *testing.T) {
	c := &Client{}

	c.SetUserSyncSeq(10)
	c.SetUserSyncSeq(9)
	if got := c.UserSyncSeq(); got != 10 {
		t.Fatalf("expected user sync seq to stay 10 after lower update, got %d", got)
	}

	c.updateUserSyncSeqFromHeader("11")
	if got := c.UserSyncSeq(); got != 11 {
		t.Fatalf("expected user sync seq to advance to 11, got %d", got)
	}

	c.updateUserSyncSeqFromHeader("8")
	if got := c.UserSyncSeq(); got != 11 {
		t.Fatalf("expected stale header to be ignored, got %d", got)
	}
}

func TestGetFullUserListSkipsIfNoneMatch(t *testing.T) {
	var gotIfNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"next"`)
		w.Header().Set("X-User-Sync-Seq", "12")
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	c := &Client{
		client:   resty.New().SetBaseURL(server.URL),
		userEtag: `"old"`,
	}

	users, err := c.GetFullUserList(context.Background())
	if err != nil {
		t.Fatalf("GetFullUserList returned error: %v", err)
	}
	if gotIfNoneMatch != "" {
		t.Fatalf("expected forced full user list to skip If-None-Match, got %q", gotIfNoneMatch)
	}
	if users == nil {
		t.Fatal("expected 200 empty users response to return a non-nil slice")
	}
	if got := c.UserSyncSeq(); got != 12 {
		t.Fatalf("expected sync seq 12, got %d", got)
	}
}
