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

func TestCheckLimitRejectsDeviceLimitByUUID(t *testing.T) {
	const tag = "device-limit-by-uuid"
	const uuid = "limited-device"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id:          9,
		Uuid:        uuid,
		DeviceLimit: 1,
	}}, nil, map[int]int{9: 1}, true)

	_, reject, info := l.CheckLimit(format.UserTag(tag, uuid), "192.0.2.30", true)
	if !reject {
		t.Fatal("expected uuid device limit to be rejected")
	}
	if info.Reason != LimitRejectReasonDeviceLimitExceeded {
		t.Fatalf("expected reason %q, got %q", LimitRejectReasonDeviceLimitExceeded, info.Reason)
	}
	if info.DeviceLimit != 1 || info.AliveCount != 1 || info.PendingDeviceCount != 1 || !info.UseDeviceLimitByUUID {
		t.Fatalf("unexpected reject info: %+v", info)
	}
}
