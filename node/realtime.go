package node

import (
	"context"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/netstat"
)

type realtimeTelemetrySender interface {
	SendTelemetry(panel.NodeTelemetryMessage) error
}

// One instance per connection: never carry permissions or sequences across reconnects.
type nodeRealtimeRuntime struct {
	refresh func(context.Context) error
	sample  func() (*netstat.Throughput, bool, error)
	send    func(panel.NodeTelemetryMessage) error
	jitter  time.Duration
}

func (r *userSyncRuntime) serveRealtime(ctx context.Context, conn userSyncWakeupConnection, policies <-chan *panel.UserSyncWakeupMessage) {
	sender, ok := conn.(realtimeTelemetrySender)
	if !ok || r.controller == nil {
		return
	}
	// Independent sampler: fast display updates must not alter the 60s legacy average.
	sampler := netstat.NewSampler()
	runtime := &nodeRealtimeRuntime{
		refresh: func(ctx context.Context) error { return r.controller.monitorNodeConfig(ctx, false) },
		sample:  sampler.Sample,
		send:    sender.SendTelemetry,
		jitter:  stableUserSyncJitter(r.instanceID),
	}
	runtime.run(ctx, policies)
}

func (r *nodeRealtimeRuntime) run(ctx context.Context, policies <-chan *panel.UserSyncWakeupMessage) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var expires, nextSample, nextFetch time.Time
	var desired, applied, inFlight string
	var interval time.Duration
	var seq int64
	var cancelFetch context.CancelFunc
	var fetchDone chan error
	backoff := time.Second
	defer func() {
		if cancelFetch != nil {
			cancelFetch()
		}
		if fetchDone != nil {
			<-fetchDone
		}
	}()
	disable := func() {
		expires = time.Time{}
		desired = ""
		applied = ""
		if cancelFetch != nil {
			cancelFetch()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case policy, ok := <-policies:
			if !ok {
				return
			}
			if policy == nil || !policy.Enabled {
				disable()
				continue
			}
			now := time.Now()
			wasExpired := !now.Before(expires)
			expires = now.Add(time.Duration(policy.LeaseSeconds) * time.Second)
			interval = time.Duration(policy.IntervalSeconds) * time.Second
			if policy.ConfigRevision != desired {
				desired = policy.ConfigRevision
				// Coalesce rapid edits and spread simultaneous hints across instances.
				nextFetch = now.Add(250*time.Millisecond + r.jitter)
				backoff = time.Second
			}
			if wasExpired {
				_, _, _ = r.sample() // establish a baseline without sending cumulative counters as a rate
				nextSample = now.Add(interval + r.jitter)
			}
		case err := <-fetchDone:
			fetchDone = nil
			cancelFetch()
			cancelFetch = nil
			if err == nil && time.Now().Before(expires) {
				applied = inFlight
				backoff = time.Second
			} else {
				nextFetch = time.Now().Add(backoff)
				backoff *= 2
				if backoff > 10*time.Second {
					backoff = 10 * time.Second
				}
			}
		case now := <-ticker.C:
			if !now.Before(expires) {
				if !expires.IsZero() {
					disable()
				}
				continue
			}
			if desired != applied && fetchDone == nil && !now.Before(nextFetch) {
				fetchCtx, cancel := context.WithDeadline(ctx, expires)
				cancelFetch = cancel
				fetchDone = make(chan error, 1)
				inFlight = desired
				go func(done chan<- error) { done <- r.refresh(fetchCtx) }(fetchDone)
			}
			if !now.Before(nextSample) {
				nextSample = now.Add(interval)
				value, ready, err := r.sample()
				if err != nil || !ready || value == nil {
					continue
				}
				seq++
				if err := r.send(panel.NodeTelemetryMessage{
					Seq: seq, RxBps: value.RxBps, TxBps: value.TxBps,
					RxBytes: value.RxBytes, TxBytes: value.TxBytes,
					SampleInterval: value.IntervalSeconds, SampledAt: value.CapturedAt.Unix(),
				}); err != nil {
					// No optional retries on a broken writer; the main connection owns reconnects.
					return
				}
			}
		}
	}
}
