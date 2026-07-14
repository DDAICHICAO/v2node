package node

import (
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type offlineTransition struct {
	Entered bool
	Warn24h bool
	Warn72h bool
	Elapsed time.Duration
}

func (c *Controller) recordPanelFailure(component, operation string, err error) {
	c.recordPanelFailureAt(component, operation, err, time.Now())
}

func (c *Controller) recordPanelFailureAt(component, operation string, err error, now time.Time) {
	transition := c.offlineTracker.Failure(component, now)
	fields := log.Fields{
		"tag":         c.tag,
		"component":   component,
		"operation":   operation,
		"err":         err,
		"offline_for": transition.Elapsed.Round(time.Second).String(),
	}
	if transition.Entered {
		log.WithFields(fields).Warning("Panel unavailable; keeping last known runtime state")
	}
	if transition.Warn24h {
		log.WithFields(fields).Warning("Panel has been unavailable for 24 hours; continuing service")
	}
	if transition.Warn72h {
		log.WithFields(fields).Error("Panel has been unavailable for 72 hours; continuing service")
	}
}

func (c *Controller) recordPanelSuccess(component string) {
	duration, recovered := c.offlineTracker.Success(component, time.Now())
	if !recovered {
		return
	}
	log.WithFields(log.Fields{
		"tag":         c.tag,
		"offline_for": duration.Round(time.Second).String(),
	}).Info("Panel communication recovered")
}

type offlineTracker struct {
	mu        sync.Mutex
	since     time.Time
	warned24h bool
	warned72h bool
	failed    map[string]struct{}
}

func (t *offlineTracker) Failure(component string, now time.Time) offlineTransition {
	t.mu.Lock()
	defer t.mu.Unlock()

	transition := offlineTransition{}
	if t.failed == nil {
		t.failed = make(map[string]struct{})
	}
	t.failed[component] = struct{}{}
	if t.since.IsZero() {
		t.since = now
		transition.Entered = true
	}
	transition.Elapsed = now.Sub(t.since)
	if transition.Elapsed < 0 {
		transition.Elapsed = 0
	}
	if transition.Elapsed >= 24*time.Hour && !t.warned24h {
		t.warned24h = true
		transition.Warn24h = true
	}
	if transition.Elapsed >= 72*time.Hour && !t.warned72h {
		t.warned72h = true
		transition.Warn72h = true
	}
	return transition
}

func (t *offlineTracker) Success(component string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.failed, component)
	if t.since.IsZero() {
		return 0, false
	}
	if len(t.failed) != 0 {
		return 0, false
	}
	duration := now.Sub(t.since)
	if duration < 0 {
		duration = 0
	}
	t.since = time.Time{}
	t.warned24h = false
	t.warned72h = false
	t.failed = nil
	return duration, true
}
