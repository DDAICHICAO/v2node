package limiter

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	uuidIPFanoutMaxAuditIPs = 256
	uuidIPFanoutMaxEvents   = 2000
)

type UUIDIPFanoutConfig struct {
	Enabled        bool
	Mode           string
	Window         time.Duration
	MaxUniqueIPs   int
	EventCooldown  time.Duration
	WhitelistCIDRs []string
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

type uuidIPObservation struct {
	firstSeen time.Time
	lastSeen  time.Time
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
	defer t.mu.Unlock()

	next := normalizeUUIDIPFanoutConfig(config)
	if !sameUUIDIPFanoutConfig(t.config, next) {
		t.windows = make(map[string]map[string]uuidIPObservation)
		t.lastEventAt = make(map[string]time.Time)
		if !next.Enabled || next.Mode != "reject" {
			t.global = make(map[string]uuidIPFanoutGlobalEntry)
			t.globalMode = ""
		}
	}
	t.updateConfigLocked(next)
}

func (t *UUIDIPFanoutTracker) Check(taguuid string, userID int, rawIP string, now time.Time, exempt bool) UUIDIPFanoutCheckResult {
	if t == nil {
		return UUIDIPFanoutCheckResult{}
	}
	ip := normalizeFanoutIP(rawIP)
	if ip == "" || taguuid == "" || userID <= 0 {
		return UUIDIPFanoutCheckResult{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	config := t.config
	if !config.Enabled || exempt || t.isTrustedLocked(ip) {
		return UUIDIPFanoutCheckResult{}
	}
	t.cleanupLocked(now)

	uuid := extractUUIDFromTagUUID(taguuid)
	if config.Mode == "reject" && t.globalMode == "reject" {
		if decision, ok := t.global[uuidIPFanoutGlobalKey(userID, uuid)]; ok {
			if decision.expiresAt > now.Unix() {
				if _, allowed := decision.allowed[hashNormalizedIP(ip)]; !allowed {
					threshold := decision.threshold
					if threshold <= 0 {
						threshold = config.MaxUniqueIPs
					}
					window := decision.window
					if window <= 0 {
						window = int(config.Window / time.Second)
					}
					return UUIDIPFanoutCheckResult{
						Reject:        true,
						Scope:         "global",
						UniqueIPCount: threshold + 1,
						Threshold:     threshold,
						WindowSeconds: window,
					}
				}
			} else {
				delete(t.global, uuidIPFanoutGlobalKey(userID, uuid))
			}
		}
	}

	window := t.windows[taguuid]
	if window == nil {
		window = make(map[string]uuidIPObservation)
		t.windows[taguuid] = window
	}
	cutoff := now.Add(-config.Window)
	for observedIP, observation := range window {
		if observation.lastSeen.Before(cutoff) {
			delete(window, observedIP)
		}
	}
	if observation, ok := window[ip]; ok {
		observation.lastSeen = now
		window[ip] = observation
		return UUIDIPFanoutCheckResult{
			Scope:          "local",
			UniqueIPCount:  len(window),
			Threshold:      config.MaxUniqueIPs,
			WindowSeconds:  int(config.Window / time.Second),
			EventGenerated: false,
		}
	}
	if len(window) < config.MaxUniqueIPs {
		window[ip] = uuidIPObservation{firstSeen: now, lastSeen: now}
		return UUIDIPFanoutCheckResult{
			Scope:         "local",
			UniqueIPCount: len(window),
			Threshold:     config.MaxUniqueIPs,
			WindowSeconds: int(config.Window / time.Second),
		}
	}

	result := UUIDIPFanoutCheckResult{
		Reject:        config.Mode == "reject",
		Scope:         "local",
		UniqueIPCount: len(window) + 1,
		Threshold:     config.MaxUniqueIPs,
		WindowSeconds: int(config.Window / time.Second),
	}
	if config.Mode == "audit" {
		if len(window) >= uuidIPFanoutMaxAuditIPs {
			delete(window, oldestUUIDIPFanoutIP(window))
			result.Truncated = true
		}
		window[ip] = uuidIPObservation{firstSeen: now, lastSeen: now}
		result.UniqueIPCount = len(window)
		if result.Truncated {
			result.UniqueIPCount++
		}
	}
	result.EventGenerated = t.enqueueEventLocked(taguuid, UUIDIPFanoutEvent{
		UserID:        userID,
		UUID:          uuid,
		IP:            ip,
		Scope:         "local",
		Action:        config.Mode,
		UniqueIPCount: result.UniqueIPCount,
		Threshold:     config.MaxUniqueIPs,
		WindowSeconds: int(config.Window / time.Second),
		Truncated:     result.Truncated,
	}, now)
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
	return config
}

func sameUUIDIPFanoutConfig(left, right UUIDIPFanoutConfig) bool {
	return left.Enabled == right.Enabled &&
		left.Mode == right.Mode &&
		left.Window == right.Window &&
		left.MaxUniqueIPs == right.MaxUniqueIPs &&
		left.EventCooldown == right.EventCooldown &&
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
