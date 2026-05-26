package core

import (
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestAddUsersDoesNotCommitUIDMapWhenBuildFails(t *testing.T) {
	v := New(nil)
	_, err := v.AddUsers(&AddUsersParams{
		Tag: "bad-node",
		Users: []panel.UserInfo{
			{Id: 1, Uuid: "new-user"},
		},
		NodeInfo: &panel.NodeInfo{Type: "unsupported"},
	})
	if err == nil {
		t.Fatal("expected unsupported node type to fail")
	}

	v.users.mapLock.RLock()
	defer v.users.mapLock.RUnlock()
	if len(v.users.uidMap) != 0 {
		t.Fatalf("expected uid map to stay empty after failed add, got %+v", v.users.uidMap)
	}
}

func TestSntpEclipseOnlineRefreshIntervalUsesBaseConfig(t *testing.T) {
	got := sntpEclipseOnlineRefreshInterval(&panel.NodeInfo{
		Common: &panel.CommonNode{
			BaseConfig: &panel.BaseConfig{
				SntpEclipseOnlineRefresh: "9",
			},
		},
	})
	if got != 9*time.Second {
		t.Fatalf("expected online refresh interval 9s, got %s", got)
	}

	if got := sntpEclipseOnlineRefreshInterval(nil); got != defaultSntpEclipseOnlineRefresh {
		t.Fatalf("expected default online refresh interval, got %s", got)
	}
}
