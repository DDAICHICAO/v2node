package node

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestApplyUserDeltaEventsUpsertReplacesUserRows(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1", SpeedLimit: 10, DeviceLimit: 1},
		{Id: 1, Uuid: "old-device-1", SpeedLimit: 10, DeviceLimit: 1},
		{Id: 2, Uuid: "legacy-2", SpeedLimit: 20, DeviceLimit: 2},
	}
	events := []panel.UserDeltaEvent{
		{
			Seq:    5,
			Action: panel.UserDeltaActionUpsert,
			UserID: 1,
			Users: []panel.UserInfo{
				{Id: 1, Uuid: "legacy-1", SpeedLimit: 30, DeviceLimit: 3},
				{Id: 1, Uuid: "new-device-1", SpeedLimit: 30, DeviceLimit: 3},
			},
		},
	}

	got, changed := applyUserDeltaEvents(oldUsers, events)
	if !changed {
		t.Fatal("expected delta to change user list")
	}

	want := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1", SpeedLimit: 30, DeviceLimit: 3},
		{Id: 1, Uuid: "new-device-1", SpeedLimit: 30, DeviceLimit: 3},
		{Id: 2, Uuid: "legacy-2", SpeedLimit: 20, DeviceLimit: 2},
	}
	assertUserListEqual(t, got, want)
}

func TestApplyUserDeltaEventsDeleteRemovesAllRowsForUserID(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "legacy-1"},
		{Id: 1, Uuid: "device-1"},
		{Id: 2, Uuid: "legacy-2"},
	}
	events := []panel.UserDeltaEvent{
		{Seq: 6, Action: panel.UserDeltaActionDelete, UserID: 1},
	}

	got, changed := applyUserDeltaEvents(oldUsers, events)
	if !changed {
		t.Fatal("expected delta to change user list")
	}

	want := []panel.UserInfo{{Id: 2, Uuid: "legacy-2"}}
	assertUserListEqual(t, got, want)
}

func TestApplyUserDeltaEventsNoEventsKeepsList(t *testing.T) {
	oldUsers := []panel.UserInfo{{Id: 1, Uuid: "legacy-1"}}

	got, changed := applyUserDeltaEvents(oldUsers, nil)
	if changed {
		t.Fatal("expected empty delta to keep user list unchanged")
	}
	assertUserListEqual(t, got, oldUsers)
}

func TestRemoveExpiredUsers(t *testing.T) {
	oldUsers := []panel.UserInfo{
		{Id: 1, Uuid: "expired", ExpiredAt: 100},
		{Id: 2, Uuid: "active", ExpiredAt: 200},
		{Id: 3, Uuid: "never-expire"},
	}

	got, changed := removeExpiredUsers(oldUsers, 100)
	if !changed {
		t.Fatal("expected expired user to be removed")
	}

	want := []panel.UserInfo{
		{Id: 2, Uuid: "active", ExpiredAt: 200},
		{Id: 3, Uuid: "never-expire"},
	}
	assertUserListEqual(t, got, want)
}

func TestUserDeltaPruneTimeRequiresPanelServerTime(t *testing.T) {
	if _, ok := userDeltaPruneTime(nil); ok {
		t.Fatal("expected nil delta to skip local prune")
	}
	if _, ok := userDeltaPruneTime(&panel.UserDeltaData{}); ok {
		t.Fatal("expected delta without panel server time to skip local prune")
	}

	got, ok := userDeltaPruneTime(&panel.UserDeltaData{ServerTime: 123})
	if !ok {
		t.Fatal("expected panel server time to enable prune")
	}
	if got != 123 {
		t.Fatalf("got prune time %d, want 123", got)
	}
}

func TestUpdateTaskIsDowngrade(t *testing.T) {
	cases := []struct {
		name    string
		current string
		target  string
		want    bool
	}{
		{name: "target older", current: "v5.0.0.24", target: "v5.0.0.23", want: true},
		{name: "target same", current: "v5.0.0.24", target: "v5.0.0.24", want: false},
		{name: "target newer", current: "v5.0.0.23", target: "v5.0.0.24", want: false},
		{name: "command output current", current: "v2node v5.0.0.24 (SNTP)", target: "v5.0.0.23", want: true},
		{name: "non numeric current", current: "dev-build", target: "v5.0.0.23", want: false},
		{name: "non numeric target", current: "v5.0.0.24", target: "dev-build", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := updateTaskIsDowngrade(tc.current, tc.target); got != tc.want {
				t.Fatalf("updateTaskIsDowngrade(%q, %q)=%v, want %v", tc.current, tc.target, got, tc.want)
			}
		})
	}
}

func assertUserListEqual(t *testing.T, got, want []panel.UserInfo) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d, len(want)=%d; got=%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Id != want[i].Id ||
			got[i].Uuid != want[i].Uuid ||
			got[i].SpeedLimit != want[i].SpeedLimit ||
			got[i].DeviceLimit != want[i].DeviceLimit ||
			got[i].ExpiredAt != want[i].ExpiredAt {
			t.Fatalf("got[%d]=%+v, want=%+v", i, got[i], want[i])
		}
	}
}
