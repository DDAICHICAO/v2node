package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultDeviceLimitEventCooldown = 10 * time.Minute
	defaultDeviceLimitEventMaxItems = 5000
)

type DeviceLimitEvent struct {
	EventID              string
	GroupScope           string
	UserID               int
	UUID                 string
	Mode                 string
	DeviceLimit          int
	AliveCount           int
	PendingDeviceCount   int
	CachedDeviceOverlap  int
	EffectiveDeviceCount int
	MaxObservedCount     int
	HitCount             int
	FirstSeenAt          int64
	LastSeenAt           int64
	OccurredAt           int64
}

type deviceLimitEventEntry struct {
	groupKey string
	event    DeviceLimitEvent
	pending  bool
}

type DeviceLimitEventQueue struct {
	mu           sync.Mutex
	cooldown     time.Duration
	maxItems     int
	records      map[string]*deviceLimitEventEntry
	latest       map[string]string
	recordOrder  []string
	pendingOrder []string
}

func NewDeviceLimitEventQueue(cooldown time.Duration, maxItems int) *DeviceLimitEventQueue {
	if cooldown <= 0 {
		cooldown = defaultDeviceLimitEventCooldown
	}
	if maxItems <= 0 {
		maxItems = defaultDeviceLimitEventMaxItems
	}
	return &DeviceLimitEventQueue{
		cooldown: cooldown,
		maxItems: maxItems,
		records:  make(map[string]*deviceLimitEventEntry),
		latest:   make(map[string]string),
	}
}

func (q *DeviceLimitEventQueue) Enqueue(event DeviceLimitEvent, now time.Time) {
	if q == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	occurredAt := event.OccurredAt
	if occurredAt <= 0 {
		occurredAt = now.Unix()
	}
	groupKey := deviceLimitEventGroupKey(event)

	q.mu.Lock()
	defer q.mu.Unlock()

	if eventID := q.latest[groupKey]; eventID != "" {
		if entry := q.records[eventID]; entry != nil &&
			occurredAt-entry.event.FirstSeenAt < int64(q.cooldown/time.Second) {
			mergeDeviceLimitEvent(&entry.event, event, occurredAt, true)
			if !entry.pending {
				entry.pending = true
				q.pendingOrder = append(q.pendingOrder, eventID)
			}
			return
		}
	}

	q.makeRoomLocked()
	event.FirstSeenAt = occurredAt
	event.LastSeenAt = occurredAt
	event.HitCount = maxInt(event.HitCount, 1)
	event.MaxObservedCount = observedDeviceCount(event)
	event.EventID = deviceLimitEventID(groupKey, occurredAt)
	event.OccurredAt = 0
	entry := &deviceLimitEventEntry{
		groupKey: groupKey,
		event:    event,
		pending:  true,
	}
	q.records[event.EventID] = entry
	q.latest[groupKey] = event.EventID
	q.recordOrder = append(q.recordOrder, event.EventID)
	q.pendingOrder = append(q.pendingOrder, event.EventID)
}

func (q *DeviceLimitEventQueue) Drain(maxItems int) []DeviceLimitEvent {
	if q == nil || maxItems <= 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	events := make([]DeviceLimitEvent, 0, minInt(maxItems, len(q.pendingOrder)))
	remaining := make([]string, 0, len(q.pendingOrder))
	for _, eventID := range q.pendingOrder {
		entry := q.records[eventID]
		if entry == nil || !entry.pending {
			continue
		}
		if len(events) >= maxItems {
			remaining = append(remaining, eventID)
			continue
		}
		entry.pending = false
		events = append(events, entry.event)
	}
	q.pendingOrder = remaining
	return events
}

func (q *DeviceLimitEventQueue) Requeue(events []DeviceLimitEvent) {
	if q == nil || len(events) == 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	front := make([]string, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.EventID == "" {
			continue
		}
		entry := q.records[event.EventID]
		if entry == nil {
			q.makeRoomLocked()
			groupKey := deviceLimitEventGroupKey(event)
			entry = &deviceLimitEventEntry{groupKey: groupKey, event: event}
			q.records[event.EventID] = entry
			q.recordOrder = append(q.recordOrder, event.EventID)
			if current := q.latest[groupKey]; current == "" {
				q.latest[groupKey] = event.EventID
			}
		} else {
			mergeDeviceLimitEvent(&entry.event, event, event.LastSeenAt, false)
		}
		if entry.pending {
			continue
		}
		entry.pending = true
		front = append(front, event.EventID)
	}
	for i, j := 0, len(front)-1; i < j; i, j = i+1, j-1 {
		front[i], front[j] = front[j], front[i]
	}
	q.pendingOrder = append(front, q.pendingOrder...)
}

func (q *DeviceLimitEventQueue) makeRoomLocked() {
	for len(q.records) >= q.maxItems && len(q.recordOrder) > 0 {
		eventID := q.recordOrder[0]
		q.recordOrder = q.recordOrder[1:]
		entry := q.records[eventID]
		if entry == nil {
			continue
		}
		delete(q.records, eventID)
		if q.latest[entry.groupKey] == eventID {
			delete(q.latest, entry.groupKey)
		}
		log.WithFields(log.Fields{
			"queue_limit": q.maxItems,
			"dropped":     1,
		}).Warn("Device limit event queue reached capacity")
	}
}

func mergeDeviceLimitEvent(target *DeviceLimitEvent, incoming DeviceLimitEvent, occurredAt int64, addHit bool) {
	if target == nil {
		return
	}
	target.AliveCount = maxInt(target.AliveCount, incoming.AliveCount)
	target.PendingDeviceCount = maxInt(target.PendingDeviceCount, incoming.PendingDeviceCount)
	target.CachedDeviceOverlap = maxInt(target.CachedDeviceOverlap, incoming.CachedDeviceOverlap)
	target.EffectiveDeviceCount = maxInt(target.EffectiveDeviceCount, incoming.EffectiveDeviceCount)
	target.MaxObservedCount = maxInt(target.MaxObservedCount, observedDeviceCount(incoming))
	if occurredAt > 0 {
		if target.FirstSeenAt <= 0 || occurredAt < target.FirstSeenAt {
			target.FirstSeenAt = occurredAt
		}
		target.LastSeenAt = maxInt64(target.LastSeenAt, occurredAt)
	}
	if addHit {
		target.HitCount += maxInt(incoming.HitCount, 1)
	} else {
		target.HitCount = maxInt(target.HitCount, incoming.HitCount)
	}
}

func observedDeviceCount(event DeviceLimitEvent) int {
	return maxInt(event.MaxObservedCount, maxInt(event.EffectiveDeviceCount, event.AliveCount+1))
}

func deviceLimitEventGroupKey(event DeviceLimitEvent) string {
	return fmt.Sprintf(
		"%s|%d|%s|%s|%d",
		event.GroupScope,
		event.UserID,
		event.UUID,
		event.Mode,
		event.DeviceLimit,
	)
}

func deviceLimitEventID(groupKey string, firstSeenAt int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", groupKey, firstSeenAt)))
	return hex.EncodeToString(sum[:])
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
