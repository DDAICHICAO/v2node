package limiter

import (
	"context"
	"sync"
	"time"
)

const (
	uuidIPFanoutBreakerFailures = 3
	uuidIPFanoutBreakerOpenFor  = 30 * time.Second
)

const (
	UUIDIPFanoutOutcomeAllow    = "allow"
	UUIDIPFanoutOutcomeReject   = "reject"
	UUIDIPFanoutOutcomeFailOpen = "fail_open"
	UUIDIPFanoutOutcomeFallback = "snapshot"
	UUIDIPFanoutOutcomeProtocol = "protocol"
)

type UUIDIPFanoutReservationOutcome struct {
	Kind          string
	Scope         string
	UniqueIPCount int
	Threshold     int
	WindowSeconds int
}

type UUIDIPFanoutReservationFunc func(
	context.Context,
	UUIDIPFanoutInspection,
) UUIDIPFanoutReservationOutcome

type uuidIPFanoutReservationState struct {
	mu             sync.Mutex
	reserve        UUIDIPFanoutReservationFunc
	failures       int
	openUntil      time.Time
	halfOpenActive bool
}

func (t *UUIDIPFanoutTracker) SetReservationFunc(reserve UUIDIPFanoutReservationFunc) {
	if t == nil {
		return
	}
	t.reservation.mu.Lock()
	t.reservation.reserve = reserve
	t.reservation.mu.Unlock()
}

func (t *UUIDIPFanoutTracker) CheckWithReservation(
	ctx context.Context,
	taguuid string,
	userID int,
	rawIP string,
	now time.Time,
	exempt bool,
) UUIDIPFanoutCheckResult {
	inspection := t.Inspect(taguuid, userID, rawIP, now, exempt)
	switch inspection.Kind {
	case UUIDIPFanoutBypass:
		return UUIDIPFanoutCheckResult{}
	case UUIDIPFanoutExisting:
		return uuidIPFanoutResultFromInspection(inspection, false)
	case UUIDIPFanoutLocalAllow:
		return t.CommitAllowed(inspection, now, false)
	case UUIDIPFanoutLocalReject:
		return t.rejectInspection(inspection, now)
	case UUIDIPFanoutNeedsReservation:
	default:
		return UUIDIPFanoutCheckResult{}
	}

	value, _, _ := t.flight.Do(
		inspection.TagUUID+"\x00"+inspection.IP,
		func() (interface{}, error) {
			timeout, enabled := t.reservationSettings()
			if !enabled {
				return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFallback}, nil
			}
			if ctx == nil {
				ctx = context.Background()
			}
			requestContext, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return t.reserveCandidate(requestContext, inspection, now), nil
		},
	)
	outcome, ok := value.(UUIDIPFanoutReservationOutcome)
	if !ok {
		outcome = UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeProtocol}
	}
	return t.applyReservationOutcome(inspection, outcome, now)
}

func (t *UUIDIPFanoutTracker) reservationSettings() (time.Duration, bool) {
	if t == nil {
		return 800 * time.Millisecond, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.config.ReservationTimeout,
		t.config.Enabled &&
			t.config.Mode == "reject" &&
			t.config.Strategy == "reservation" &&
			t.config.ReservationEnabled
}

func (t *UUIDIPFanoutTracker) reserveCandidate(
	ctx context.Context,
	inspection UUIDIPFanoutInspection,
	now time.Time,
) UUIDIPFanoutReservationOutcome {
	reserve, circuitOpen := t.beginReservation(now)
	if reserve == nil {
		if circuitOpen {
			return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFailOpen}
		}
		return UUIDIPFanoutReservationOutcome{Kind: UUIDIPFanoutOutcomeFallback}
	}

	outcome := reserve(ctx, inspection)
	switch outcome.Kind {
	case UUIDIPFanoutOutcomeAllow,
		UUIDIPFanoutOutcomeReject,
		UUIDIPFanoutOutcomeFailOpen,
		UUIDIPFanoutOutcomeFallback,
		UUIDIPFanoutOutcomeProtocol:
	default:
		outcome.Kind = UUIDIPFanoutOutcomeProtocol
	}
	t.finishReservation(outcome.Kind, now)
	return outcome
}

func (t *UUIDIPFanoutTracker) beginReservation(now time.Time) (UUIDIPFanoutReservationFunc, bool) {
	t.reservation.mu.Lock()
	defer t.reservation.mu.Unlock()
	if t.reservation.reserve == nil {
		return nil, false
	}
	if t.reservation.failures < uuidIPFanoutBreakerFailures {
		return t.reservation.reserve, false
	}
	if t.reservation.openUntil.After(now) || t.reservation.halfOpenActive {
		return nil, true
	}
	t.reservation.halfOpenActive = true
	return t.reservation.reserve, false
}

func (t *UUIDIPFanoutTracker) finishReservation(kind string, now time.Time) {
	t.reservation.mu.Lock()
	defer t.reservation.mu.Unlock()
	t.reservation.halfOpenActive = false
	if kind == UUIDIPFanoutOutcomeFailOpen {
		t.reservation.failures++
		if t.reservation.failures >= uuidIPFanoutBreakerFailures {
			t.reservation.openUntil = now.Add(uuidIPFanoutBreakerOpenFor)
		}
		return
	}
	t.reservation.failures = 0
	t.reservation.openUntil = time.Time{}
}

func (t *UUIDIPFanoutTracker) applyReservationOutcome(
	inspection UUIDIPFanoutInspection,
	outcome UUIDIPFanoutReservationOutcome,
	now time.Time,
) UUIDIPFanoutCheckResult {
	switch outcome.Kind {
	case UUIDIPFanoutOutcomeAllow:
		return t.CommitAllowed(inspection, now, false)
	case UUIDIPFanoutOutcomeReject:
		return uuidIPFanoutResultFromOutcome(inspection, outcome, "global")
	case UUIDIPFanoutOutcomeFailOpen:
		return t.CommitAllowed(inspection, now, true)
	case UUIDIPFanoutOutcomeFallback:
		return t.applySnapshotFallback(inspection, now)
	case UUIDIPFanoutOutcomeProtocol:
		return uuidIPFanoutResultFromOutcome(inspection, outcome, "protocol")
	default:
		return uuidIPFanoutResultFromOutcome(inspection, outcome, "protocol")
	}
}

func (t *UUIDIPFanoutTracker) applySnapshotFallback(
	original UUIDIPFanoutInspection,
	now time.Time,
) UUIDIPFanoutCheckResult {
	inspection := t.inspect(
		original.TagUUID,
		original.UserID,
		original.IP,
		now,
		false,
		true,
	)
	switch inspection.Kind {
	case UUIDIPFanoutBypass:
		return UUIDIPFanoutCheckResult{}
	case UUIDIPFanoutExisting:
		return uuidIPFanoutResultFromInspection(inspection, false)
	case UUIDIPFanoutLocalAllow:
		return t.CommitAllowed(inspection, now, false)
	case UUIDIPFanoutLocalReject:
		return t.rejectInspection(inspection, now)
	default:
		return t.CommitAllowed(inspection, now, false)
	}
}

func (t *UUIDIPFanoutTracker) rejectInspection(
	inspection UUIDIPFanoutInspection,
	now time.Time,
) UUIDIPFanoutCheckResult {
	result := uuidIPFanoutResultFromInspection(inspection, true)
	if inspection.Scope != "local" {
		return result
	}

	t.mu.Lock()
	defer t.mu.Unlock()
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

func uuidIPFanoutResultFromInspection(
	inspection UUIDIPFanoutInspection,
	reject bool,
) UUIDIPFanoutCheckResult {
	return UUIDIPFanoutCheckResult{
		Reject:        reject,
		Scope:         inspection.Scope,
		UniqueIPCount: inspection.UniqueIPCount,
		Threshold:     inspection.Threshold,
		WindowSeconds: inspection.WindowSeconds,
	}
}

func uuidIPFanoutResultFromOutcome(
	inspection UUIDIPFanoutInspection,
	outcome UUIDIPFanoutReservationOutcome,
	scope string,
) UUIDIPFanoutCheckResult {
	uniqueIPCount := outcome.UniqueIPCount
	if uniqueIPCount <= 0 {
		uniqueIPCount = inspection.UniqueIPCount
	}
	threshold := outcome.Threshold
	if threshold <= 0 {
		threshold = inspection.Threshold
	}
	windowSeconds := outcome.WindowSeconds
	if windowSeconds <= 0 {
		windowSeconds = inspection.WindowSeconds
	}
	if outcome.Scope == "global" || outcome.Scope == "protocol" {
		scope = outcome.Scope
	}
	return UUIDIPFanoutCheckResult{
		Reject:        true,
		Scope:         scope,
		UniqueIPCount: uniqueIPCount,
		Threshold:     threshold,
		WindowSeconds: windowSeconds,
	}
}
