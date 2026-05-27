package limiter

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

func newTestLimiter(tag string, users []panel.UserInfo, alive map[int]int, deviceAlive map[int]int, useDeviceLimitByUUID bool) *Limiter {
	Init()
	return AddLimiter("v2ray", tag, users, alive, deviceAlive, useDeviceLimitByUUID)
}

func TestCheckLimitRejectsUnknownUser(t *testing.T) {
	const tag = "unknown-user"
	l := newTestLimiter(tag, nil, nil, nil, false)

	_, reject, info := l.CheckLimit(format.UserTag(tag, "missing"), "::ffff:192.0.2.1", true)
	if !reject {
		t.Fatal("expected unknown user to be rejected")
	}
	if info.Reason != LimitRejectReasonUserNotFound {
		t.Fatalf("expected reason %q, got %q", LimitRejectReasonUserNotFound, info.Reason)
	}
	if info.IP != "192.0.2.1" {
		t.Fatalf("expected normalized ip, got %q", info.IP)
	}
}

func TestCheckLimitRejectsBlockedIP(t *testing.T) {
	const tag = "blocked-ip"
	const uuid = "blocked-user"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:         42,
		Uuid:       uuid,
		BlockedIPs: []string{"192.0.2.10"},
	}}, nil, nil, false)

	_, reject, info := l.CheckLimit(format.UserTag(tag, uuid), "::ffff:192.0.2.10", true)
	if !reject {
		t.Fatal("expected blocked ip to be rejected")
	}
	if info.Reason != LimitRejectReasonBlockedIP {
		t.Fatalf("expected reason %q, got %q", LimitRejectReasonBlockedIP, info.Reason)
	}
	if info.UID != 42 {
		t.Fatalf("expected uid 42, got %d", info.UID)
	}
}

func TestCheckLimitRejectsDeviceLimit(t *testing.T) {
	const tag = "device-limit"
	const uuid = "limited-user"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:          7,
		Uuid:        uuid,
		DeviceLimit: 1,
	}}, map[int]int{7: 1}, nil, false)

	_, reject, info := l.CheckLimit(format.UserTag(tag, uuid), "192.0.2.20", true)
	if !reject {
		t.Fatal("expected device/ip limit to be rejected")
	}
	if info.Reason != LimitRejectReasonDeviceLimitExceeded {
		t.Fatalf("expected reason %q, got %q", LimitRejectReasonDeviceLimitExceeded, info.Reason)
	}
	if info.DeviceLimit != 1 || info.AliveCount != 1 || info.UseDeviceLimitByUUID {
		t.Fatalf("unexpected reject info: %+v", info)
	}
}

func TestCheckLimitAllowsUUIDDeviceOverlapFromBackendCache(t *testing.T) {
	const tag = "device-limit-by-uuid"
	const uuid = "limited-device"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:          9,
		Uuid:        uuid,
		DeviceLimit: 1,
	}}, nil, map[int]int{9: 1}, true)

	_, reject, info := l.CheckLimit(format.UserTag(tag, uuid), "192.0.2.30", true)
	if reject {
		t.Fatalf("expected backend cache overlap to be allowed, got reject info: %+v", info)
	}
}

func TestMarkOnlineRefreshesStateAfterSnapshot(t *testing.T) {
	const tag = "long-lived-session"
	const uuid = "sntp-eclipse-device"
	taguuid := format.UserTag(tag, uuid)
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:   12,
		Uuid: uuid,
	}}, nil, nil, true)

	if !l.MarkOnline(taguuid, "::ffff:192.0.2.60") {
		t.Fatal("expected online marker to be accepted")
	}
	onlineUsers, onlineDevices, err := l.GetOnlineDeviceState()
	if err != nil {
		t.Fatalf("expected first online snapshot: %v", err)
	}
	if len(*onlineUsers) != 1 || len(*onlineDevices) != 1 {
		t.Fatalf("expected first snapshot to include one online user/device, got users=%d devices=%d", len(*onlineUsers), len(*onlineDevices))
	}

	if !l.MarkOnline(taguuid, "192.0.2.60") {
		t.Fatal("expected refreshed online marker to be accepted")
	}
	onlineUsers, onlineDevices, err = l.GetOnlineDeviceState()
	if err != nil {
		t.Fatalf("expected refreshed online snapshot: %v", err)
	}
	if len(*onlineUsers) != 1 || len(*onlineDevices) != 1 {
		t.Fatalf("expected refreshed snapshot to include one online user/device, got users=%d devices=%d", len(*onlineUsers), len(*onlineDevices))
	}
	if (*onlineUsers)[0].UID != 12 || (*onlineUsers)[0].IP != "192.0.2.60" {
		t.Fatalf("unexpected refreshed online user: %+v", (*onlineUsers)[0])
	}
	if (*onlineDevices)[0].UUID != uuid {
		t.Fatalf("expected device uuid %q, got %+v", uuid, (*onlineDevices)[0])
	}
}

func TestRefreshOnlineUIDsFromLastSnapshotKeepsLongLivedTrafficOnline(t *testing.T) {
	const tag = "long-lived-anytls"
	const uuid = "device-a"
	taguuid := format.UserTag(tag, uuid)
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:   12,
		Uuid: uuid,
	}}, nil, nil, true)

	_, reject, info := l.CheckLimit(taguuid, "192.0.2.80", true)
	if reject {
		t.Fatalf("expected first long-lived connection to be accepted, got reject info: %+v", info)
	}
	onlineUsers, onlineDevices, err := l.GetOnlineDeviceState()
	if err != nil {
		t.Fatalf("expected first online snapshot: %v", err)
	}
	if len(*onlineUsers) != 1 || len(*onlineDevices) != 1 {
		t.Fatalf("expected first snapshot to include one user/device, got users=%d devices=%d", len(*onlineUsers), len(*onlineDevices))
	}

	refreshed := l.RefreshOnlineUIDsFromLastSnapshot([]int{12})
	if refreshed != 1 {
		t.Fatalf("expected one refreshed long-lived device, got %d", refreshed)
	}
	onlineUsers, onlineDevices, err = l.GetOnlineDeviceState()
	if err != nil {
		t.Fatalf("expected refreshed online snapshot: %v", err)
	}
	if len(*onlineUsers) != 1 || len(*onlineDevices) != 1 {
		t.Fatalf("expected traffic refresh to keep one user/device online, got users=%d devices=%d", len(*onlineUsers), len(*onlineDevices))
	}
	if (*onlineUsers)[0].UID != 12 || (*onlineUsers)[0].IP != "192.0.2.80" {
		t.Fatalf("unexpected refreshed online user: %+v", (*onlineUsers)[0])
	}
	if (*onlineDevices)[0].UUID != uuid {
		t.Fatalf("expected device uuid %q, got %+v", uuid, (*onlineDevices)[0])
	}
}

func TestAnyTLSRecordsOnlineEvenWhenNetworkFlagIsNotTCP(t *testing.T) {
	const tag = "anytls-online"
	const uuid = "device-a"
	taguuid := format.UserTag(tag, uuid)
	Init()
	l := AddLimiter("anytls", tag, []panel.UserInfo{{
		Id:   12,
		Uuid: uuid,
	}}, nil, nil, true)

	_, reject, info := l.CheckLimit(taguuid, "192.0.2.81", false)
	if reject {
		t.Fatalf("expected anytls connection to be accepted, got reject info: %+v", info)
	}
	onlineUsers, onlineDevices, err := l.GetOnlineDeviceState()
	if err != nil {
		t.Fatalf("expected online snapshot: %v", err)
	}
	if len(*onlineUsers) != 1 || len(*onlineDevices) != 1 {
		t.Fatalf("expected anytls snapshot to include one user/device, got users=%d devices=%d", len(*onlineUsers), len(*onlineDevices))
	}
	if (*onlineUsers)[0].UID != 12 || (*onlineUsers)[0].IP != "192.0.2.81" {
		t.Fatalf("unexpected anytls online user: %+v", (*onlineUsers)[0])
	}
}

func TestCheckLimitRejectsUUIDDeviceLimitWhenPendingExceedsLimit(t *testing.T) {
	const tag = "uuid-pending-limit"
	l := newTestLimiter(tag, []panel.UserInfo{
		{
			Id:          9,
			Uuid:        "device-a",
			DeviceLimit: 1,
		},
		{
			Id:          9,
			Uuid:        "device-b",
			DeviceLimit: 1,
		},
	}, nil, map[int]int{9: 1}, true)

	if _, reject, info := l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.31", true); reject {
		t.Fatalf("expected first pending uuid to be treated as cache overlap, got reject info: %+v", info)
	}

	_, reject, info := l.CheckLimit(format.UserTag(tag, "device-b"), "192.0.2.32", true)
	if !reject {
		t.Fatal("expected second pending uuid to be rejected")
	}
	if info.Reason != LimitRejectReasonDeviceLimitExceeded {
		t.Fatalf("expected reason %q, got %q", LimitRejectReasonDeviceLimitExceeded, info.Reason)
	}
	if info.DeviceLimit != 1 || info.AliveCount != 1 || info.PendingDeviceCount != 2 || info.CachedDeviceOverlap != 1 || info.EffectiveDeviceCount != 2 || !info.UseDeviceLimitByUUID {
		t.Fatalf("unexpected reject info: %+v", info)
	}
}

func TestUpdateAliveStateCopiesAndSwitchesDeviceMode(t *testing.T) {
	const tag = "alive-state-snapshot"
	const uuid = "limited-user"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:          9,
		Uuid:        uuid,
		DeviceLimit: 1,
	}}, map[int]int{9: 1}, nil, false)

	alive := map[int]int{9: 0}
	deviceAlive := map[int]int{9: 1}
	l.UpdateAliveState(alive, deviceAlive, true)
	alive[9] = 99
	deviceAlive[9] = 99

	_, reject, info := l.CheckLimit(format.UserTag(tag, uuid), "192.0.2.50", true)
	if reject {
		t.Fatalf("expected copied device alive overlap to allow first pending device, got %+v", info)
	}
	if !info.UseDeviceLimitByUUID && info.Reason != LimitRejectReasonNone {
		t.Fatalf("unexpected reject info after device alive update: %+v", info)
	}

	l.UpdateAliveState(map[int]int{9: 1}, nil, false)
	_, reject, info = l.CheckLimit(format.UserTag(tag, "another-device"), "192.0.2.51", true)
	if !reject || info.Reason != LimitRejectReasonUserNotFound || info.UseDeviceLimitByUUID {
		t.Fatalf("expected missing user reject with device mode disabled, got reject=%v info=%+v", reject, info)
	}
}

func TestUpdateUserDeletingOneUUIDKeepsUIDAliveCache(t *testing.T) {
	const tag = "delete-one-device"
	l := newTestLimiter(tag, []panel.UserInfo{
		{Id: 9, Uuid: "device-a", DeviceLimit: 1},
		{Id: 9, Uuid: "device-b", DeviceLimit: 1},
	}, map[int]int{9: 1}, map[int]int{9: 1}, true)

	l.UpdateUser(tag, nil, []panel.UserInfo{{Id: 9, Uuid: "device-b"}}, nil)

	_, reject, info := l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.70", true)
	if reject {
		t.Fatalf("expected remaining device to keep backend alive overlap, got %+v", info)
	}
}

func TestUpdateUserDisablesExistingBucketWhenSpeedLimitRemoved(t *testing.T) {
	const tag = "speed-limit-removed"
	const uuid = "limited-user"
	taguuid := format.UserTag(tag, uuid)
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:         11,
		Uuid:       uuid,
		SpeedLimit: 10,
	}}, nil, nil, false)

	bucket, reject, info := l.CheckLimit(taguuid, "192.0.2.40", true)
	if reject {
		t.Fatalf("expected limited user to pass, got reject info: %+v", info)
	}
	if bucket == nil || bucket.Get() == nil {
		t.Fatal("expected speed limiter bucket")
	}

	l.UpdateUser(tag, nil, nil, []panel.UserInfo{{
		Id:   11,
		Uuid: uuid,
	}})

	if bucket.Get() != nil {
		t.Fatal("expected existing bucket to be disabled after speed limit removal")
	}
	if _, ok := l.SpeedLimiter.Load(taguuid); ok {
		t.Fatal("expected speed limiter map entry to be removed")
	}

	nextBucket, reject, info := l.CheckLimit(taguuid, "192.0.2.40", true)
	if reject {
		t.Fatalf("expected unlimited user to pass, got reject info: %+v", info)
	}
	if nextBucket != nil {
		t.Fatal("expected no speed limiter for new connections after speed limit removal")
	}
}
