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
