package cmd

import "time"

// Owned by serverHandle: polling, file changes and timed recovery share one gate.
type reloadRetryState struct {
	digest string
	delay  time.Duration
	next   time.Time
}

func (r *reloadRetryState) Pending() bool   { return !r.next.IsZero() }
func (r *reloadRetryState) Next() time.Time { return r.next }

func (r *reloadRetryState) Allow(now time.Time, digest string) bool {
	return !r.Pending() || digest != r.digest || !now.Before(r.next)
}

func (r *reloadRetryState) Failed(now time.Time, digest string) {
	if digest != r.digest || r.delay == 0 {
		r.delay = 30 * time.Second
	} else {
		r.delay = min(2*r.delay, 5*time.Minute)
	}
	r.digest, r.next = digest, now.Add(r.delay)
}

func (r *reloadRetryState) Succeeded() { *r = reloadRetryState{} }
