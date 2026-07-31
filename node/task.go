package node

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	vCore "github.com/wyx2685/v2node/core"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	c.startUserExpiryScheduler()
	c.startUserSyncRuntime(node)
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:            "nodeInfoMonitor",
		Interval:        node.PullInterval,
		Execute:         c.nodeInfoMonitor,
		ReloadCh:        c.server.ReloadCh,
		ReloadOnTimeout: false,
	}
	c.aliveStatePeriodic = &task.Task{
		Name:            "refreshAliveStateTask",
		Interval:        c.aliveStateRefreshInterval(),
		Execute:         c.refreshAliveStateTask,
		ReloadCh:        c.server.ReloadCh,
		ReloadOnTimeout: false,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:            "reportUserTrafficTask",
		Interval:        node.PushInterval,
		Execute:         c.reportUserTrafficTask,
		ReloadCh:        c.server.ReloadCh,
		ReloadOnTimeout: false,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start alive state refresh")
	_ = c.aliveStatePeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:            "renewCertTask",
				Interval:        time.Hour * 24,
				Execute:         c.renewCertTask,
				ReloadCh:        c.server.ReloadCh,
				ReloadOnTimeout: true,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	if c.pendingNodeInfo != nil {
		if err := c.persistOfflineState(c.pendingNodeInfo); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Persist pending node configuration failed")
			return err
		}
		if err := c.queueReload(); err != nil {
			return err
		}
		c.pendingNodeInfo = nil
		c.recordPanelSuccess("config")
		return nil
	}

	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		c.recordPanelFailure("config", "get node info", err)
		return fmt.Errorf("get node info: %w", err)
	}
	if newN != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Info("Got new node info; persist before reload")
		c.pendingNodeInfo = newN
		if err := c.persistOfflineState(newN); err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Persist new node configuration failed; reload deferred")
			return err
		}
		if err := c.queueReload(); err != nil {
			return err
		}
		c.pendingNodeInfo = nil
		c.recordPanelSuccess("config")
		return nil
	}
	log.WithField("tag", c.tag).Debug("Node info no change")
	c.checkUpdateTask(ctx)
	c.checkStreamUnlockTask(ctx)
	c.recordPanelSuccess("config")
	return nil
}

func (c *Controller) queueReload() error {
	if c.server == nil || c.server.ReloadCh == nil {
		return errors.New("reload channel is nil")
	}
	select {
	case c.server.ReloadCh <- struct{}{}:
	default:
	}
	return nil
}

type userDeltaFetcher func(context.Context, int64) (*panel.UserDeltaData, error)

type fullSyncBreaker struct {
	failures  int
	openUntil time.Time
}

func (b *fullSyncBreaker) Failure(now time.Time) {
	b.failures++
	if b.failures >= 3 {
		b.openUntil = now.Add(30 * time.Second)
	}
}

func (b *fullSyncBreaker) Success() {
	b.failures = 0
	b.openUntil = time.Time{}
}

func (b *fullSyncBreaker) OpenUntil(now time.Time) time.Time {
	if !b.openUntil.After(now) {
		return time.Time{}
	}
	return b.openUntil
}

func collectUserDeltaPages(
	ctx context.Context,
	sinceSeq int64,
	maxPages int,
	fetch userDeltaFetcher,
) (*panel.UserDeltaData, error) {
	combined := &panel.UserDeltaData{LatestSeq: sinceSeq}
	current := sinceSeq
	for page := 0; page < maxPages; page++ {
		delta, err := fetch(ctx, current)
		if err != nil {
			return nil, err
		}
		if delta == nil || delta.LatestSeq < current {
			return nil, fmt.Errorf("invalid user delta page after seq %d", current)
		}
		if delta.FullRequired {
			return delta, nil
		}
		if delta.HasMore && delta.LatestSeq <= current {
			return nil, fmt.Errorf(
				"user delta page did not advance after seq %d",
				current,
			)
		}
		combined.Events = append(combined.Events, delta.Events...)
		combined.LatestSeq = delta.LatestSeq
		combined.HasMore = delta.HasMore
		combined.ServerTime = delta.ServerTime
		current = delta.LatestSeq
		if !delta.HasMore {
			return combined, nil
		}
	}
	return combined, nil
}

func commitUserStateWith(
	previous []panel.UserInfo,
	next []panel.UserInfo,
	nextSeq int64,
	apply func([]panel.UserInfo) error,
	persist func([]panel.UserInfo, int64) error,
	setSeq func(int64),
) error {
	if err := apply(next); err != nil {
		return err
	}
	if err := persist(next, nextSeq); err != nil {
		if rollbackErr := apply(previous); rollbackErr != nil {
			return fmt.Errorf(
				"persist user snapshot: %w; rollback: %v",
				err,
				rollbackErr,
			)
		}
		return fmt.Errorf("persist user snapshot: %w", err)
	}
	setSeq(nextSeq)
	return nil
}

func (c *Controller) commitUserState(
	next []panel.UserInfo,
	nextSeq int64,
) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	previous := append([]panel.UserInfo(nil), c.userList...)
	err := commitUserStateWith(
		previous,
		append([]panel.UserInfo(nil), next...),
		nextSeq,
		c.applyUserList,
		func(users []panel.UserInfo, seq int64) error {
			return c.persistOfflineStateAtLocked(c.info, users, seq)
		},
		c.apiClient.SetUserSyncSeq,
	)
	if err == nil {
		c.notifyUserExpiryScheduler()
	}
	return err
}

func (c *Controller) commitFetchedUserState(
	next []panel.UserInfo,
	snapshot *panel.UserListSnapshot,
) error {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	previous := append([]panel.UserInfo(nil), c.userList...)
	err := commitUserStateWith(
		previous,
		append([]panel.UserInfo(nil), next...),
		snapshot.SyncSeq,
		c.applyUserList,
		func(users []panel.UserInfo, seq int64) error {
			return c.persistOfflineStateAtLocked(c.info, users, seq)
		},
		func(int64) {
			c.apiClient.CommitUserListSnapshot(snapshot)
		},
	)
	if err == nil {
		c.notifyUserExpiryScheduler()
	}
	return err
}

func (c *Controller) syncUserState(
	ctx context.Context,
) (committedSeq int64, syncErr error) {
	c.userSyncMu.Lock()
	defer c.userSyncMu.Unlock()
	defer func() {
		if syncErr == nil {
			c.recordPanelSuccess("sync")
		} else if !isContextError(syncErr) {
			c.recordPanelFailure("sync", "sync user state", syncErr)
		}
	}()

	currentSeq := c.apiClient.UserSyncSeq()
	delta, err := collectUserDeltaPages(
		ctx,
		currentSeq,
		10,
		c.apiClient.GetUserDeltaSince,
	)
	forceFullUserList := false
	if err == nil && delta != nil && !delta.FullRequired {
		if validateErr := validateUserDelta(delta); validateErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": validateErr,
			}).Warn("User delta invalid, fallback to full user list")
			forceFullUserList = true
		} else {
			pruneUnix, canPrune := userDeltaPruneTime(delta)
			newU, changed := applyUserDeltaEvents(c.userList, delta.Events)
			if canPrune {
				var expired bool
				newU, expired = removeExpiredUsers(newU, pruneUnix)
				changed = changed || expired
			}
			if !changed && delta.LatestSeq == currentSeq {
				log.WithField("tag", c.tag).Debug("User delta no applicable change")
				return currentSeq, nil
			}
			if err := c.commitUserState(newU, delta.LatestSeq); err != nil {
				return currentSeq, err
			}
			if delta.HasMore {
				return delta.LatestSeq, &panel.UserSyncRetryError{
					StatusCode: 503,
					After:      500 * time.Millisecond,
				}
			}
			return delta.LatestSeq, nil
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return currentSeq, err
		}
		if !errors.Is(err, panel.ErrUserDeltaUnsupported) {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Warn("Get user delta failed")
			return currentSeq, err
		}
	} else if delta != nil && delta.FullRequired {
		log.WithField("tag", c.tag).Info("User delta requires full user list")
		forceFullUserList = true
	}

	// get user info
	if forceFullUserList {
		now := time.Now()
		if until := c.fullSyncBreaker.OpenUntil(now); until.After(now) {
			return currentSeq, &panel.UserSyncRetryError{
				StatusCode: 503,
				After:      until.Sub(now),
			}
		}
	}
	snapshot, err := c.apiClient.FetchUserList(ctx, forceFullUserList)
	if err != nil {
		if forceFullUserList {
			c.fullSyncBreaker.Failure(time.Now())
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return currentSeq, fmt.Errorf("get user list: %w", err)
	}

	// node no changed, check users
	if snapshot.NotModified && snapshot.SyncSeq == currentSeq {
		log.WithField("tag", c.tag).Debug("User list no change")
		return currentSeq, nil
	}
	newU := c.userList
	if !snapshot.NotModified {
		newU = snapshot.Users
	}
	if pruneUnix, canPrune := userDeltaPruneTime(delta); canPrune {
		newU, _ = removeExpiredUsers(newU, pruneUnix)
	}
	if err := c.commitFetchedUserState(newU, snapshot); err != nil {
		if forceFullUserList {
			c.fullSyncBreaker.Failure(time.Now())
		}
		return currentSeq, err
	}
	if forceFullUserList {
		c.fullSyncBreaker.Success()
	}
	return snapshot.SyncSeq, nil
}

func (c *Controller) aliveStateRefreshInterval() time.Duration {
	const minInterval = 30 * time.Second
	if c.info != nil && c.info.PullInterval > minInterval {
		return c.info.PullInterval
	}
	return minInterval
}

func (c *Controller) refreshAliveState(ctx context.Context) error {
	newA, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		return fmt.Errorf("get alive list: %w", err)
	}
	if newA == nil {
		return errors.New("get alive list: panel returned no data")
	}

	useDeviceLimitByUUID := c.supportsDeviceLimitByUUID()
	newDeviceAlive := make(map[int]int)
	var newUUIDIPFanout panel.UUIDIPFanoutGlobalState
	hasDeviceState := useDeviceLimitByUUID || c.supportsDeviceAliveReport()
	if hasDeviceState {
		deviceState, deviceErr := c.apiClient.GetUserDeviceAliveState(ctx)
		if deviceErr != nil {
			err = deviceErr
			return fmt.Errorf("get device alive list: %w", err)
		}
		if deviceState == nil || deviceState.AliveDevices == nil {
			return errors.New("get device alive list: panel returned no data")
		}
		newDeviceAlive = deviceState.AliveDevices
		newUUIDIPFanout = deviceState.UUIDIPFanout
	}
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.aliveMap = cloneIntMap(newA)
	c.deviceAliveMap = cloneIntMap(newDeviceAlive)
	if c.limiter != nil {
		c.limiter.UpdateAliveState(newA, newDeviceAlive, useDeviceLimitByUUID)
		if hasDeviceState && c.limiter.UpdateUUIDIPFanoutGlobal(uuidIPFanoutLimiterGlobal(newUUIDIPFanout), time.Now()) {
			c.uuidIPFanoutGlobal = cloneUUIDIPFanoutGlobalState(newUUIDIPFanout)
		}
	}
	return nil
}

func (c *Controller) refreshAliveStateTask(ctx context.Context) error {
	if err := c.refreshAliveState(ctx); err != nil {
		c.recordPanelFailure("alive", "refresh alive state", err)
		return err
	}
	if err := c.persistOfflineState(c.info); err != nil {
		c.recordPanelFailure("alive", "persist alive state", err)
		return err
	}
	c.recordPanelSuccess("alive")
	return nil
}

func (c *Controller) applyUserList(newU []panel.UserInfo) error {
	deleted, added, modified := compareUserList(c.userList, newU)
	if len(added) > 0 {
		// have added users
		_, err := c.server.AddUsers(&vCore.AddUsersParams{
			Tag:      c.tag,
			NodeInfo: c.info,
			Users:    added,
		})
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Add users failed")
			return err
		}
	}
	if len(deleted) > 0 {
		// have deleted users
		err := c.server.DelUsers(deleted, c.tag, c.info)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Delete users failed")
			if len(added) > 0 {
				if rollbackErr := c.server.DelUsers(added, c.tag, c.info); rollbackErr != nil {
					log.WithFields(log.Fields{
						"tag": c.tag,
						"err": rollbackErr,
					}).Error("Rollback added users failed")
				}
			}
			return err
		}
	}
	if len(added) > 0 || len(deleted) > 0 || len(modified) > 0 {
		// update Limiter
		c.limiter.UpdateUser(c.tag, added, deleted, modified)
		if len(modified) > 0 {
			c.closeBlockedUserIPs(modified)
		}
	}
	c.userList = newU
	log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	return nil
}
