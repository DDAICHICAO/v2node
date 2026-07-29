package limiter

import (
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
)

func TestDeviceLimitEventQueueMergesCooldownHitsAndKeepsStableID(t *testing.T) {
	queue := NewDeviceLimitEventQueue(10*time.Minute, 5000)
	now := time.Unix(1000, 0)
	queue.Enqueue(DeviceLimitEvent{
		UserID: 7, UUID: "device-a", Mode: "uuid",
		DeviceLimit: 1, EffectiveDeviceCount: 2, OccurredAt: now.Unix(),
	}, now)
	first := queue.Drain(500)
	if len(first) != 1 {
		t.Fatalf("first drain events=%+v", first)
	}

	queue.Enqueue(DeviceLimitEvent{
		UserID: 7, UUID: "device-a", Mode: "uuid",
		DeviceLimit: 1, EffectiveDeviceCount: 3, OccurredAt: now.Add(time.Minute).Unix(),
	}, now.Add(time.Minute))
	events := queue.Drain(500)
	if len(events) != 1 || events[0].HitCount != 2 {
		t.Fatalf("events=%+v", events)
	}
	if len(events[0].EventID) != 64 || events[0].EventID != first[0].EventID {
		t.Fatalf("event ids first=%q second=%q", first[0].EventID, events[0].EventID)
	}
	if events[0].MaxObservedCount != 3 {
		t.Fatalf("event=%+v", events[0])
	}
}

func TestDeviceLimitEventQueueRequeuesAtFrontWithoutDuplicatingNewerHits(t *testing.T) {
	queue := NewDeviceLimitEventQueue(10*time.Minute, 4)
	now := time.Unix(1000, 0)
	queue.Enqueue(DeviceLimitEvent{
		UserID: 7, UUID: "device-a", Mode: "uuid",
		DeviceLimit: 1, EffectiveDeviceCount: 2,
	}, now)
	failed := queue.Drain(500)
	queue.Enqueue(DeviceLimitEvent{
		UserID: 7, UUID: "device-a", Mode: "uuid",
		DeviceLimit: 1, EffectiveDeviceCount: 3,
	}, now.Add(time.Minute))
	queue.Requeue(failed)

	events := queue.Drain(500)
	if len(events) != 1 || events[0].HitCount != 2 || events[0].MaxObservedCount != 3 {
		t.Fatalf("events=%+v", events)
	}
}

func TestDeviceLimitEventQueueSeparatesNodeScopes(t *testing.T) {
	queue := NewDeviceLimitEventQueue(10*time.Minute, 4)
	now := time.Unix(1000, 0)
	for _, scope := range []string{"node-a", "node-b"} {
		queue.Enqueue(DeviceLimitEvent{
			GroupScope: scope,
			UserID:     7, UUID: "device-a", Mode: "uuid",
			DeviceLimit: 1, EffectiveDeviceCount: 2,
		}, now)
	}

	events := queue.Drain(500)
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	if events[0].EventID == events[1].EventID {
		t.Fatalf("node scopes reused event id %q", events[0].EventID)
	}
}

func TestCheckLimitEnqueuesOnlyDeviceLimitRejections(t *testing.T) {
	const tag = "device-limit-event-integration"
	l := newTestLimiter(tag, []panel.UserInfo{
		{Id: 9, Uuid: "device-a", DeviceLimit: 1},
		{Id: 9, Uuid: "device-b", DeviceLimit: 1},
	}, nil, map[int]int{9: 1}, true)

	if _, reject, info := l.CheckLimit(
		format.UserTag(tag, "device-a"), "192.0.2.31", true,
	); reject {
		t.Fatalf("first device rejected: %+v", info)
	}
	if events := l.DrainDeviceLimitEvents(500); len(events) != 0 {
		t.Fatalf("accepted connection emitted events: %+v", events)
	}

	_, reject, info := l.CheckLimit(
		format.UserTag(tag, "device-b"), "192.0.2.32", true,
	)
	if !reject || info.Reason != LimitRejectReasonDeviceLimitExceeded {
		t.Fatalf("reject=%v info=%+v", reject, info)
	}
	events := l.DrainDeviceLimitEvents(500)
	if len(events) != 1 || events[0].UserID != 9 ||
		events[0].UUID != "device-b" || events[0].DeviceLimit != 1 {
		t.Fatalf("events=%+v", events)
	}
}

func TestFanoutAndBlockedIPRejectionsDoNotEmitDeviceLimitEvents(t *testing.T) {
	const tag = "device-limit-event-other-reasons"
	l := newTestLimiter(tag, []panel.UserInfo{{
		Id: 11, Uuid: "device-a", DeviceLimit: 1, BlockedIPs: []string{"192.0.2.1"},
	}}, nil, nil, true)

	_, rejected, info := l.CheckLimit(format.UserTag(tag, "device-a"), "192.0.2.1", true)
	if !rejected || info.Reason != LimitRejectReasonBlockedIP {
		t.Fatalf("rejected=%v info=%+v", rejected, info)
	}
	if events := l.DrainDeviceLimitEvents(500); len(events) != 0 {
		t.Fatalf("non-device rejection emitted events: %+v", events)
	}
}
