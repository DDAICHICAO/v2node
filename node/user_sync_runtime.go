package node

import (
	"context"
	"errors"
	"hash/crc32"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

type userSyncMode string

const (
	userSyncModeLegacy     userSyncMode = "legacy_poll"
	userSyncModeConnecting userSyncMode = "connecting"
	userSyncModeCatchingUp userSyncMode = "catching_up"
	userSyncModeFallback   userSyncMode = "fallback"
	userSyncModePush       userSyncMode = "push"
	userSyncModeClosed     userSyncMode = "closed"
)

type userSyncWakeupConnection interface {
	ReadMessage(context.Context) (*panel.UserSyncWakeupMessage, error)
	SendPong(int64) error
	Close() error
}

type userSyncWakeupTransportError struct {
	err error
}

func (e *userSyncWakeupTransportError) Error() string {
	if e == nil || e.err == nil {
		return "user sync wakeup transport error"
	}
	return e.err.Error()
}

func (e *userSyncWakeupTransportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

type userSyncRuntime struct {
	controller     *Controller
	config         *panel.UserSyncWakeupConfig
	legacyInterval time.Duration
	instanceID     string

	mu                  sync.Mutex
	mode                userSyncMode
	highestRevision     int64
	lastAppliedRevision int64
	lastWakeupWarningAt time.Time
	ackFn               func(context.Context, panel.UserSyncAppliedMessage) error

	syncFn func(context.Context) (int64, error)
	nowFn  func() time.Time
}

func newUserSyncRuntime(
	controller *Controller,
	node *panel.NodeInfo,
) *userSyncRuntime {
	var config *panel.UserSyncWakeupConfig
	if node != nil && node.Common != nil && node.Common.BaseConfig != nil &&
		node.Common.BaseConfig.UserSyncWakeup != nil {
		value := *node.Common.BaseConfig.UserSyncWakeup
		value.Normalize()
		config = &value
	}
	legacyInterval := 60 * time.Second
	if node != nil && node.PullInterval > 0 {
		legacyInterval = node.PullInterval
	}
	return &userSyncRuntime{
		controller:     controller,
		config:         config,
		legacyInterval: legacyInterval,
		instanceID:     controller.apiClient.InstanceID(),
		mode:           userSyncModeLegacy,
		syncFn:         controller.syncUserState,
		nowFn:          time.Now,
	}
}

func (r *userSyncRuntime) Run(ctx context.Context) {
	defer r.setMode(userSyncModeClosed)
	if r.config == nil || !r.config.Enabled {
		r.runLegacy(ctx)
		return
	}
	r.runWakeup(ctx)
}

func (r *userSyncRuntime) runLegacy(ctx context.Context) {
	r.setMode(userSyncModeLegacy)
	timer := time.NewTimer(r.pollInterval(nil))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			nextPoll := r.pollInterval(nil)
			if _, err := r.syncFn(ctx); err != nil {
				if !isContextError(err) {
					log.WithError(err).WithField("mode", r.modeValue()).
						Warn("User sync legacy poll failed")
				}
				if retryAfter, ok := r.retryDelay(err); ok {
					nextPoll = retryAfter
				}
			}
			timer.Reset(nextPoll)
		}
	}
}

func (r *userSyncRuntime) runWakeup(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		r.setMode(userSyncModeConnecting)
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := r.controller.apiClient.DialUserSyncWakeup(
			dialCtx,
			r.config,
		)
		cancel()
		if err == nil {
			r.setAckFn(func(
				_ context.Context,
				message panel.UserSyncAppliedMessage,
			) error {
				return conn.SendApplied(
					message.Revision,
					message.SyncSeq,
					message.AppliedAt,
				)
			})
			var reachedPush bool
			reachedPush, err = r.serveConnection(ctx, conn)
			if reachedPush {
				attempt = 0
			}
			r.setAckFn(nil)
			_ = conn.Close()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !isContextError(err) &&
			r.shouldWarnWakeupUnavailable() {
			log.WithError(err).WithField("mode", "fallback").
				Warn("User sync wakeup unavailable; using short polling")
		}
		r.setMode(userSyncModeFallback)
		reconnectAfter := r.reconnectDelay(attempt)
		initialPollAfter := time.Duration(0)
		if retryAfter, ok := r.retryDelay(err); ok {
			initialPollAfter = retryAfter
			if retryAfter > reconnectAfter {
				reconnectAfter = retryAfter
			}
		}
		if !r.runFallbackUntil(ctx, reconnectAfter, initialPollAfter) {
			return
		}
		if attempt < 5 {
			attempt++
		}
	}
}

func (r *userSyncRuntime) serveConnection(
	ctx context.Context,
	conn userSyncWakeupConnection,
) (reachedPush bool, serveErr error) {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	messages := make(chan *panel.UserSyncWakeupMessage, 8)
	readErrors := make(chan error, 1)
	go func() {
		for {
			readTimeout := time.Duration(r.config.HeartbeatSeconds*3) * time.Second
			if readTimeout < 30*time.Second {
				readTimeout = 30 * time.Second
			}
			readCtx, readCancel := context.WithTimeout(serveCtx, readTimeout)
			message, err := conn.ReadMessage(readCtx)
			readCancel()
			if err != nil {
				select {
				case readErrors <- err:
				case <-serveCtx.Done():
				}
				return
			}
			select {
			case messages <- message:
			case <-serveCtx.Done():
				return
			}
		}
	}()

	r.setMode(userSyncModeCatchingUp)
	mergeTimer := time.NewTimer(
		time.Duration(r.config.MergeMS) * time.Millisecond,
	)
	mergeC := mergeTimer.C
	catchUpRetryPending := false
	defer func() {
		mergeTimer.Stop()
	}()
	resetMergeTimer := func(delay time.Duration) {
		if !mergeTimer.Stop() {
			select {
			case <-mergeTimer.C:
			default:
			}
		}
		mergeTimer.Reset(delay)
		mergeC = mergeTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			_ = conn.Close()
			return reachedPush, ctx.Err()
		case err := <-readErrors:
			return reachedPush, err
		case message := <-messages:
			switch message.Type {
			case "ping":
				if err := conn.SendPong(r.now().Unix()); err != nil {
					return reachedPush, err
				}
			case "user_sync_dirty":
				r.enqueueRevision(message.Revision)
				if !catchUpRetryPending {
					resetMergeTimer(
						time.Duration(r.config.MergeMS) * time.Millisecond,
					)
				}
			}
		case <-mergeC:
			mergeC = nil
			catchUpRetryPending = false
			var err error
			if r.pendingRevision() > 0 {
				err = r.flushPending(ctx)
			} else {
				err = r.activatePush(ctx)
			}
			if err != nil {
				if ctx.Err() != nil {
					return reachedPush, ctx.Err()
				}
				var transportErr *userSyncWakeupTransportError
				if errors.As(err, &transportErr) {
					return reachedPush, err
				}
				r.setMode(userSyncModeCatchingUp)
				retryAfter := r.fallbackInterval()
				if delay, ok := r.retryDelay(err); ok {
					retryAfter = delay
				}
				catchUpRetryPending = true
				resetMergeTimer(retryAfter)
				continue
			}
			r.setMode(userSyncModePush)
			reachedPush = true
		}
	}
}

func (r *userSyncRuntime) runFallbackUntil(
	ctx context.Context,
	reconnectAfter time.Duration,
	initialPollAfter time.Duration,
) bool {
	deadline := time.NewTimer(reconnectAfter)
	defer deadline.Stop()
	poll := time.NewTimer(initialPollAfter)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return true
		case <-poll.C:
			nextPoll := r.fallbackInterval()
			if _, err := r.syncFn(ctx); err != nil {
				if !isContextError(err) {
					log.WithError(err).WithField("mode", r.modeValue()).
						Debug("User sync fallback poll failed")
				}
				if retryAfter, ok := r.retryDelay(err); ok {
					nextPoll = retryAfter
				}
			}
			poll.Reset(nextPoll)
		}
	}
}

func (r *userSyncRuntime) enqueueRevision(revision int64) {
	if revision <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision > r.highestRevision && revision > r.lastAppliedRevision {
		r.highestRevision = revision
	}
}

func (r *userSyncRuntime) flushPending(ctx context.Context) error {
	r.mu.Lock()
	revision := r.highestRevision
	ack := r.ackFn
	r.mu.Unlock()
	if revision <= 0 {
		return nil
	}
	if r.syncFn == nil {
		return errors.New("user sync function is unavailable")
	}
	seq, err := r.syncFn(ctx)
	if err != nil {
		return err
	}
	if ack == nil {
		return &userSyncWakeupTransportError{
			err: errors.New("user sync acknowledgement channel is unavailable"),
		}
	}
	message := panel.UserSyncAppliedMessage{
		Type:      "user_sync_applied",
		Revision:  revision,
		SyncSeq:   seq,
		AppliedAt: r.now().Unix(),
	}
	if err := ack(ctx, message); err != nil {
		return &userSyncWakeupTransportError{err: err}
	}
	r.mu.Lock()
	if r.highestRevision <= revision {
		r.highestRevision = 0
	}
	if revision > r.lastAppliedRevision {
		r.lastAppliedRevision = revision
	}
	r.mu.Unlock()
	return nil
}

func (r *userSyncRuntime) activatePush(ctx context.Context) error {
	r.setMode(userSyncModeCatchingUp)
	if r.syncFn == nil {
		r.setMode(userSyncModeFallback)
		return errors.New("user sync function is unavailable")
	}
	if _, err := r.syncFn(ctx); err != nil {
		r.setMode(userSyncModeFallback)
		return err
	}
	r.setMode(userSyncModePush)
	return nil
}

func (r *userSyncRuntime) pollInterval(
	config *panel.UserSyncWakeupConfig,
) time.Duration {
	if config == nil || !config.Enabled {
		if r.legacyInterval > 0 {
			return r.legacyInterval
		}
		return 60 * time.Second
	}
	return time.Duration(config.FallbackPollMS)*time.Millisecond +
		stableUserSyncJitter(r.instanceID)
}

func (r *userSyncRuntime) fallbackInterval() time.Duration {
	return r.pollInterval(r.config)
}

func (r *userSyncRuntime) reconnectDelay(attempt int) time.Duration {
	delays := [...]time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
	}
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(delays) {
		attempt = len(delays) - 1
	}
	return delays[attempt] + stableUserSyncJitter(r.instanceID)
}

func (r *userSyncRuntime) retryDelay(err error) (time.Duration, bool) {
	var retry *panel.UserSyncRetryError
	if !errors.As(err, &retry) || retry == nil {
		return 0, false
	}
	delay := retry.After
	if delay < 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	return delay + stableUserSyncJitter(r.instanceID), true
}

func stableUserSyncJitter(instanceID string) time.Duration {
	return time.Duration(crc32.ChecksumIEEE([]byte(instanceID))%500) *
		time.Millisecond
}

func (r *userSyncRuntime) setAckFn(
	ack func(context.Context, panel.UserSyncAppliedMessage) error,
) {
	r.mu.Lock()
	r.ackFn = ack
	r.mu.Unlock()
}

func (r *userSyncRuntime) setMode(mode userSyncMode) {
	r.mu.Lock()
	r.mode = mode
	r.mu.Unlock()
}

func (r *userSyncRuntime) modeValue() userSyncMode {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode
}

func (r *userSyncRuntime) pendingRevision() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.highestRevision
}

func (r *userSyncRuntime) shouldWarnWakeupUnavailable() bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastWakeupWarningAt.IsZero() &&
		now.Sub(r.lastWakeupWarningAt) < time.Minute {
		return false
	}
	r.lastWakeupWarningAt = now
	return true
}

func (r *userSyncRuntime) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (c *Controller) startUserSyncRuntime(node *panel.NodeInfo) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := newUserSyncRuntime(c, node)
	done := make(chan struct{})
	c.userSyncCancel = cancel
	c.userSyncDone = done
	c.userSyncRuntime = runtime
	go func() {
		defer close(done)
		runtime.Run(ctx)
	}()
}

func (c *Controller) closeUserSyncRuntime() {
	if c.userSyncCancel == nil {
		return
	}
	c.userSyncCancel()
	if c.userSyncDone != nil {
		select {
		case <-c.userSyncDone:
		case <-time.After(5 * time.Second):
			log.WithField("tag", c.tag).
				Warn("Timed out waiting for user sync runtime to stop")
		}
	}
	c.userSyncCancel = nil
	c.userSyncDone = nil
}
