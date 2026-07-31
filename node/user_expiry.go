package node

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

func nextUserExpiry(
	users []panel.UserInfo,
	now int64,
) (int64, bool) {
	var next int64
	for _, user := range users {
		if user.ExpiredAt == 0 {
			continue
		}
		if user.ExpiredAt <= now {
			return now, true
		}
		if next == 0 || user.ExpiredAt < next {
			next = user.ExpiredAt
		}
	}
	return next, next > 0
}

func (c *Controller) startUserExpiryScheduler() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	c.expiryWakeCh = make(chan struct{}, 1)
	c.expiryCancel = cancel
	c.expiryDone = done
	go func() {
		defer close(done)
		c.runUserExpiryScheduler(ctx)
	}()
}

func (c *Controller) runUserExpiryScheduler(ctx context.Context) {
	for {
		expiry, ok := c.currentNextUserExpiry(time.Now().Unix())
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-c.expiryWakeCh:
				continue
			}
		}

		delay := time.Until(time.Unix(expiry, 0))
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.expiryWakeCh:
			timer.Stop()
			continue
		case <-timer.C:
		}

		if err := c.expireUsersAt(ctx, time.Now().Unix()); err != nil {
			if isContextError(err) {
				return
			}
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Warn("Apply local user expiry failed; retrying")
			retry := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				retry.Stop()
				return
			case <-c.expiryWakeCh:
				retry.Stop()
			case <-retry.C:
			}
		}
	}
}

func (c *Controller) currentNextUserExpiry(now int64) (int64, bool) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return nextUserExpiry(c.userList, now)
}

func (c *Controller) expireUsersAt(
	ctx context.Context,
	now int64,
) error {
	c.userSyncMu.Lock()
	defer c.userSyncMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	c.stateMu.Lock()
	current := append([]panel.UserInfo(nil), c.userList...)
	seq := c.apiClient.UserSyncSeq()
	c.stateMu.Unlock()

	next, changed := removeExpiredUsers(current, now)
	if !changed {
		return nil
	}
	log.WithField("tag", c.tag).Info("Remove locally expired users")
	return c.commitUserState(next, seq)
}

func (c *Controller) notifyUserExpiryScheduler() {
	if c.expiryWakeCh == nil {
		return
	}
	select {
	case c.expiryWakeCh <- struct{}{}:
	default:
	}
}

func (c *Controller) cancelUserExpiryScheduler() {
	if c.expiryCancel != nil {
		c.expiryCancel()
	}
}

func (c *Controller) waitUserExpiryScheduler() {
	if c.expiryDone != nil {
		select {
		case <-c.expiryDone:
		case <-time.After(5 * time.Second):
			log.WithField("tag", c.tag).
				Warn("Timed out waiting for user expiry scheduler to stop")
		}
	}
	c.expiryCancel = nil
	c.expiryDone = nil
	c.expiryWakeCh = nil
}
