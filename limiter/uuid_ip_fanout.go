package limiter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	uuidIPFanoutMaxAuditIPs = 256
	uuidIPFanoutMaxEvents   = 2000
)

type UUIDIPFanoutConfig struct {
	Enabled            bool
	Mode               string
	Window             time.Duration
	MaxUniqueIPs       int
	EventCooldown      time.Duration
	WhitelistCIDRs     []string
	Strategy           string
	ReservationEnabled bool
	ReservationTimeout time.Duration
}

type UUIDIPFanoutGlobalState struct {
	Revision int64
	Mode     string
	States   []UUIDIPFanoutGlobalDecision
}

type UUIDIPFanoutGlobalDecision struct {
	UserID            int
	UUID              string
	AllowedIPHashes   []string
	DecisionExpiresAt int64
	WindowSeconds     int
	Threshold         int
}

type UUIDIPFanoutEvent struct {
	UserID        int
	UUID          string
	IP            string
	Scope         string
	Action        string
	UniqueIPCount int
	Threshold     int
	WindowSeconds int
	Truncated     bool
}

type UUIDIPFanoutCheckResult struct {
	Reject         bool
	Scope          string
	UniqueIPCount  int
	Threshold      int
	WindowSeconds  int
	EventGenerated bool
	Truncated      bool
}

type UUIDIPFanoutInspectionKind uint8

const (
	UUIDIPFanoutBypass UUIDIPFanoutInspectionKind = iota
	UUIDIPFanoutExisting
	UUIDIPFanoutLocalAllow
	UUIDIPFanoutLocalReject
	UUIDIPFanoutNeedsReservation
)

type UUIDIPFanoutInspection struct {
	Kind          UUIDIPFanoutInspectionKind
	TagUUID       string
	UserID        int
	UUID          string
	IP            string
	Scope         string
	UniqueIPCount int
	Threshold     int
	WindowSeconds int
}

type uuidIPObservation struct {
	firstSeen  time.Time
	lastSeen   time.Time
	unverified bool
}

type uuidIPFanoutGlobalEntry struct {
	allowed   map[string]struct{}
	expiresAt int64
	threshold int
	window    int
}

type UUIDIPFanoutTracker struct {
	mu                sync.Mutex
	config            UUIDIPFanoutConfig
	trusted           []*net.IPNet
	windows           map[string]map[string]uuidIPObservation
	global            map[string]uuidIPFanoutGlobalEntry
	globalRevision    int64
	globalInitialized bool
	globalMode        string
	lastEventAt       map[string]time.Time
	events            []UUIDIPFanoutEvent
	lastCleanup       time.Time
	reservation       uuidIPFanoutReservationState
	flight            singleflight.Group
}

func NewUUIDIPFanoutTracker(config UUIDIPFanoutConfig) *UUIDIPFanoutTracker {
	tracker := &UUIDIPFanoutTracker{
		windows:     make(map[string]map[string]uuidIPObservation),
		global:      make(map[string]uuidIPFanoutGlobalEntry),
		lastEventAt: make(map[string]time.Time),
	}
	tracker.updateConfigLocked(normalizeUUIDIPFanoutConfig(config))
	return tracker
}

func (t *UUIDIPFanoutTracker) UpdateConfig(config UUIDIPFanoutConfig) {
	if t == nil {
		return
	}
	t.mu.Lock()
	next := normalizeUUIDIPFanoutConfig(config)
	changed := !sameUUIDIPFanoutConfig(t.config, next)
	if changed {
		t.windows = make(map[string]map[string]uuidIPObservation)
		t.lastEventAt = make(map[string]time.Time)
		if !next.Enabled || next.Mode != "reject" {
			t.global = make(map[string]uuidIPFanoutGlobalEntry)
			t.globalMode = ""
		}
	}
	t.updateConfigLocked(next)
	t.mu.Unlock()

	if changed {
		t.reservation.mu.Lock()
		t.reservation.failures = 0
		t.reservation.openUntil = time.Time{}
		t.reservation.halfOpenActive = false
		t.reservation.mu.Unlock()
	}
}

func (t *UUIDIPFanoutTracker) Check(taguuid string, userID int, rawIP string, now time.Time, exempt bool) UUIDIPFanoutCheckResult {
	return t.CheckWithReservation(context.Background(), taguuid, userID, rawIP, now, exempt)
}

func (t *UUIDIPFanoutTracker) Inspect(
	taguuid string,
	userID int,
	rawIP string,
	now time.Time,
	exempt bool,
) UUIDIPFanoutInspection {
	return t.inspect(taguuid, userID, rawIP, now, exempt, false)
}

func (t *UUIDIPFanoutTracker) inspect(
	taguuid string,
	userID int,
	rawIP string,
	now time.Time,
	exempt bool,
	forceSnapshot bool,
) UUIDIPFanoutInspection {
	inspection := UUIDIPFanoutInspection{Kind: UUIDIPFanoutBypass}
	if t == nil {
		return inspection
	}
	ip := normalizeFanoutIP(rawIP)
	if ip == "" || taguuid == "" || userID <= 0 {
		return inspection
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	config := t.config
	if !config.Enabled || exempt || t.isTrustedLocked(ip) {
		return inspection
	}
	t.cleanupLocked(now)

	uuid := extractUUIDFromTagUUID(taguuid)
	windowSeconds := int(config.Window / time.Second)
	inspection = UUIDIPFanoutInspection{
		TagUUID:       taguuid,
		UserID:        userID,
		UUID:          uuid,
		IP:            ip,
		Scope:         "local",
		Threshold:     config.MaxUniqueIPs,
		WindowSeconds: windowSeconds,
	}
	useSnapshot := forceSnapshot || config.Strategy != "reservation" || !config.ReservationEnabled
	if useSnapshot && config.Mode == "reject" && t.globalMode == "reject" {
		key := uuidIPFanoutGlobalKey(userID, uuid)
		if decision, ok := t.global[key]; ok {
			if decision.expiresAt > now.Unix() {
				if _, allowed := decision.allowed[hashNormalizedIP(ip)]; !allowed {
					threshold := decision.threshold
					if threshold <= 0 {
						threshold = config.MaxUniqueIPs
					}
					window := decision.window
					if window <= 0 {
						window = windowSeconds
					}
					inspection.Kind = UUIDIPFanoutLocalReject
					inspection.Scope = "global"
					inspection.UniqueIPCount = threshold + 1
					inspection.Threshold = threshold
					inspection.WindowSeconds = window
					return inspection
				}
			} else {
				delete(t.global, key)
			}
		}
	}

	window := t.windows[taguuid]
	if window == nil {
		window = make(map[string]uuidIPObservation)
		t.windows[taguuid] = window
	}
	pruneUUIDIPFanoutWindow(window, now.Add(-config.Window))
	if observation, ok := window[ip]; ok {
		observation.lastSeen = now
		window[ip] = observation
		inspection.Kind = UUIDIPFanoutExisting
		inspection.UniqueIPCount = len(window)
		return inspection
	}

	inspection.UniqueIPCount = len(window) + 1
	if config.Mode == "reject" && len(window) >= config.MaxUniqueIPs {
		inspection.Kind = UUIDIPFanoutLocalReject
		return inspection
	}
	if config.Mode == "reject" && !useSnapshot {
		inspection.Kind = UUIDIPFanoutNeedsReservation
		return inspection
	}
	inspection.Kind = UUIDIPFanoutLocalAllow
	return inspection
}

func (t *UUIDIPFanoutTracker) CommitAllowed(
	inspection UUIDIPFanoutInspection,
	now time.Time,
	unverified bool,
) UUIDIPFanoutCheckResult {
	if t == nil || inspection.Kind == UUIDIPFanoutBypass {
		return UUIDIPFanoutCheckResult{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	config := t.config
	if !config.Enabled || inspection.TagUUID == "" || inspection.IP == "" {
		return UUIDIPFanoutCheckResult{}
	}

	t.cleanupLocked(now)
	window := t.windows[inspection.TagUUID]
	if window == nil {
		window = make(map[string]uuidIPObservation)
		t.windows[inspection.TagUUID] = window
	}
	pruneUUIDIPFanoutWindow(window, now.Add(-config.Window))
	if observation, ok := window[inspection.IP]; ok {
		observation.lastSeen = now
		observation.unverified = observation.unverified && unverified
		window[inspection.IP] = observation
		return UUIDIPFanoutCheckResult{
			Scope:         "local",
			UniqueIPCount: len(window),
			Threshold:     config.MaxUniqueIPs,
			WindowSeconds: int(config.Window / time.Second),
		}
	}

	result := UUIDIPFanoutCheckResult{
		Scope:         "local",
		UniqueIPCount: len(window) + 1,
		Threshold:     config.MaxUniqueIPs,
		WindowSeconds: int(config.Window / time.Second),
	}
	if config.Mode == "reject" && len(window) >= config.MaxUniqueIPs {
		result.Reject = true
		result.EventGenerated = t.enqueueEventLocked(inspection.TagUUID, UUIDIPFanoutEvent{
			UserID:        inspection.UserID,
			UUID:          inspection.UUID,
			IP:            inspection.IP,
			Scope:         "local",
			Action:        "reject",
			UniqueIPCount: result.UniqueIPCount,
			Threshold:     result.Threshold,
			WindowSeconds: result.WindowSeconds,
		}, now)
		return result
	}

	if config.Mode == "audit" && len(window) >= uuidIPFanoutMaxAuditIPs {
		delete(window, oldestUUIDIPFanoutIP(window))
		result.Truncated = true
	}
	window[inspection.IP] = uuidIPObservation{
		firstSeen:  now,
		lastSeen:   now,
		unverified: unverified,
	}
	result.UniqueIPCount = len(window)
	if result.Truncated {
		result.UniqueIPCount++
	}

	if unverified {
		result.Scope = UUIDIPFanoutOutcomeFailOpen
		result.EventGenerated = t.enqueueEventLocked(inspection.TagUUID, UUIDIPFanoutEvent{
			UserID:        inspection.UserID,
			UUID:          inspection.UUID,
			IP:            inspection.IP,
			Scope:         "reservation_fail_open",
			Action:        "audit",
			UniqueIPCount: result.UniqueIPCount,
			Threshold:     result.Threshold,
			WindowSeconds: result.WindowSeconds,
			Truncated:     result.Truncated,
		}, now)
	} else if config.Mode == "audit" && result.UniqueIPCount > config.MaxUniqueIPs {
		result.EventGenerated = t.enqueueEventLocked(inspection.TagUUID, UUIDIPFanoutEvent{
			UserID:        inspection.UserID,
			UUID:          inspection.UUID,
			IP:            inspection.IP,
			Scope:         "local",
			Action:        "audit",
			UniqueIPCount: result.UniqueIPCount,
			Threshold:     result.Threshold,
			WindowSeconds: result.WindowSeconds,
			Truncated:     result.Truncated,
		}, now)
	}
	return result
}

func (t *UUIDIPFanoutTracker) UpdateGlobal(state UUIDIPFanoutGlobalState, now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.globalInitialized && state.Revision <= t.globalRevision {
		return false
	}
	t.globalInitialized = true
	t.globalRevision = state.Revision
	t.globalMode = strings.ToLower(strings.TrimSpace(state.Mode))
	next := make(map[string]uuidIPFanoutGlobalEntry)
	if t.config.Enabled && t.config.Mode == "reject" && t.globalMode == "reject" {
		for _, state := range state.States {
			if state.UserID <= 0 || strings.TrimSpace(state.UUID) == "" || state.DecisionExpiresAt <= now.Unix() {
				continue
			}
			allowed := make(map[string]struct{}, len(state.AllowedIPHashes))
			for _, hash := range state.AllowedIPHashes {
				hash = strings.ToLower(strings.TrimSpace(hash))
				if len(hash) == sha256.Size*2 {
					allowed[hash] = struct{}{}
				}
			}
			next[uuidIPFanoutGlobalKey(state.UserID, state.UUID)] = uuidIPFanoutGlobalEntry{
				allowed:   allowed,
				expiresAt: state.DecisionExpiresAt,
				threshold: state.Threshold,
				window:    state.WindowSeconds,
			}
		}
	}
	t.global = next
	return true
}

func (t *UUIDIPFanoutTracker) Delete(taguuid string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.windows, taguuid)
	delete(t.lastEventAt, taguuid+"|audit")
	delete(t.lastEventAt, taguuid+"|reject")
}

func (t *UUIDIPFanoutTracker) DrainEvents(limit int) []UUIDIPFanoutEvent {
	if t == nil || limit <= 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if limit > len(t.events) {
		limit = len(t.events)
	}
	events := append([]UUIDIPFanoutEvent(nil), t.events[:limit]...)
	t.events = append([]UUIDIPFanoutEvent(nil), t.events[limit:]...)
	return events
}

func (t *UUIDIPFanoutTracker) RequeueEvents(events []UUIDIPFanoutEvent) {
	if t == nil || len(events) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	combined := append(append([]UUIDIPFanoutEvent(nil), events...), t.events...)
	if len(combined) > uuidIPFanoutMaxEvents {
		combined = combined[:uuidIPFanoutMaxEvents]
	}
	t.events = combined
}

func (t *UUIDIPFanoutTracker) updateConfigLocked(config UUIDIPFanoutConfig) {
	t.config = config
	t.trusted = parseUUIDIPFanoutCIDRs(config.WhitelistCIDRs)
	if !config.Enabled {
		t.windows = make(map[string]map[string]uuidIPObservation)
		t.global = make(map[string]uuidIPFanoutGlobalEntry)
		t.events = nil
	}
}

func (t *UUIDIPFanoutTracker) isTrustedLocked(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, network := range t.trusted {
		if network.Contains(parsed) {
			return true
		}
	}
	return false
}

func (t *UUIDIPFanoutTracker) enqueueEventLocked(taguuid string, event UUIDIPFanoutEvent, now time.Time) bool {
	key := taguuid + "|" + event.Action
	if last, ok := t.lastEventAt[key]; ok && now.Sub(last) < t.config.EventCooldown {
		return false
	}
	t.lastEventAt[key] = now
	if len(t.events) >= uuidIPFanoutMaxEvents {
		t.events = append([]UUIDIPFanoutEvent(nil), t.events[len(t.events)-uuidIPFanoutMaxEvents+1:]...)
	}
	t.events = append(t.events, event)
	return true
}

func (t *UUIDIPFanoutTracker) cleanupLocked(now time.Time) {
	interval := t.config.Window
	if interval > time.Minute {
		interval = time.Minute
	}
	if interval <= 0 {
		interval = time.Minute
	}
	if !t.lastCleanup.IsZero() && now.Sub(t.lastCleanup) < interval {
		return
	}
	cutoff := now.Add(-t.config.Window)
	for key, window := range t.windows {
		for ip, observation := range window {
			if observation.lastSeen.Before(cutoff) {
				delete(window, ip)
			}
		}
		if len(window) == 0 {
			delete(t.windows, key)
		}
	}
	for key, decision := range t.global {
		if decision.expiresAt <= now.Unix() {
			delete(t.global, key)
		}
	}
	for key, last := range t.lastEventAt {
		if now.Sub(last) >= t.config.EventCooldown {
			delete(t.lastEventAt, key)
		}
	}
	t.lastCleanup = now
}

func normalizeUUIDIPFanoutConfig(config UUIDIPFanoutConfig) UUIDIPFanoutConfig {
	config.Mode = strings.ToLower(strings.TrimSpace(config.Mode))
	if config.Mode != "reject" {
		config.Mode = "audit"
	}
	config.Strategy = strings.ToLower(strings.TrimSpace(config.Strategy))
	if config.Strategy != "reservation" {
		config.Strategy = "snapshot"
	}
	if config.Window < time.Minute {
		config.Window = 10 * time.Minute
	}
	if config.Window > time.Hour {
		config.Window = time.Hour
	}
	if config.MaxUniqueIPs < 2 {
		config.MaxUniqueIPs = 10
	}
	if config.MaxUniqueIPs > 255 {
		config.MaxUniqueIPs = 255
	}
	if config.EventCooldown < time.Minute {
		config.EventCooldown = 10 * time.Minute
	}
	if config.EventCooldown > 24*time.Hour {
		config.EventCooldown = 24 * time.Hour
	}
	normalized := make([]string, 0, len(config.WhitelistCIDRs))
	seen := make(map[string]struct{})
	for _, cidr := range config.WhitelistCIDRs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if _, ok := seen[cidr]; ok {
			continue
		}
		seen[cidr] = struct{}{}
		normalized = append(normalized, cidr)
	}
	sort.Strings(normalized)
	config.WhitelistCIDRs = normalized
	switch {
	case config.ReservationTimeout <= 0:
		config.ReservationTimeout = 800 * time.Millisecond
	case config.ReservationTimeout < 200*time.Millisecond:
		config.ReservationTimeout = 200 * time.Millisecond
	case config.ReservationTimeout > 2*time.Second:
		config.ReservationTimeout = 2 * time.Second
	}
	if !config.Enabled || config.Mode != "reject" || config.Strategy != "reservation" {
		config.ReservationEnabled = false
	}
	return config
}

func sameUUIDIPFanoutConfig(left, right UUIDIPFanoutConfig) bool {
	return left.Enabled == right.Enabled &&
		left.Mode == right.Mode &&
		left.Window == right.Window &&
		left.MaxUniqueIPs == right.MaxUniqueIPs &&
		left.EventCooldown == right.EventCooldown &&
		left.Strategy == right.Strategy &&
		left.ReservationEnabled == right.ReservationEnabled &&
		left.ReservationTimeout == right.ReservationTimeout &&
		strings.Join(left.WhitelistCIDRs, "\x00") == strings.Join(right.WhitelistCIDRs, "\x00")
}

func parseUUIDIPFanoutCIDRs(values []string) []*net.IPNet {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		}
		_, network, err := net.ParseCIDR(value)
		if err == nil {
			result = append(result, network)
		}
	}
	return result
}

func normalizeFanoutIP(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return ""
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	return ip.String()
}

func hashNormalizedIP(ip string) string {
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}

func uuidIPFanoutGlobalKey(userID int, uuid string) string {
	return itoaPositive(userID) + "\x00" + strings.ToLower(strings.TrimSpace(uuid))
}

func itoaPositive(value int) string {
	if value <= 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}

func oldestUUIDIPFanoutIP(window map[string]uuidIPObservation) string {
	oldestIP := ""
	var oldest uuidIPObservation
	for ip, observation := range window {
		if oldestIP == "" ||
			observation.lastSeen.Before(oldest.lastSeen) ||
			(observation.lastSeen.Equal(oldest.lastSeen) && ip < oldestIP) {
			oldestIP = ip
			oldest = observation
		}
	}
	return oldestIP
}

func pruneUUIDIPFanoutWindow(window map[string]uuidIPObservation, cutoff time.Time) {
	for observedIP, observation := range window {
		if observation.lastSeen.Before(cutoff) {
			delete(window, observedIP)
		}
	}
}
