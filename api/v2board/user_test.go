package panel

import "testing"

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
