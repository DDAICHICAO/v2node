package node

import (
	"context"
	"errors"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/task"
	vCore "github.com/wyx2685/v2node/core"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: c.server.ReloadCh,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: c.server.ReloadCh,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
				ReloadCh: c.server.ReloadCh,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		return nil
	}
	if newN != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Error("Got new node info, reload")
		if c.server.ReloadCh != nil {
			select {
			case c.server.ReloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
	}
	log.WithField("tag", c.tag).Debug("Node info no change")
	c.checkUpdateTask(ctx)
	c.checkStreamUnlockTask(ctx)

	return c.syncUserState(ctx)
}

func (c *Controller) syncUserState(ctx context.Context) error {
	delta, err := c.apiClient.GetUserDelta(ctx)
	forceFullUserList := false
	if err == nil && delta != nil && !delta.FullRequired {
		if validateErr := validateUserDelta(delta); validateErr != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": validateErr,
			}).Warn("User delta invalid, fallback to full user list")
			forceFullUserList = true
		} else {
			if err := c.refreshAliveStateIfDue(ctx, false); err != nil {
				return err
			}
			pruneUnix, canPrune := userDeltaPruneTime(delta)
			if len(delta.Events) == 0 {
				log.WithField("tag", c.tag).Debug("User delta no change")
				if canPrune {
					if err := c.pruneExpiredUsers(pruneUnix); err != nil {
						return err
					}
				}
				c.apiClient.SetUserSyncSeq(delta.LatestSeq)
				return nil
			}
			newU, changed := applyUserDeltaEvents(c.userList, delta.Events)
			if !changed {
				log.WithField("tag", c.tag).Debug("User delta no applicable change")
				if canPrune {
					if err := c.pruneExpiredUsers(pruneUnix); err != nil {
						return err
					}
				}
				c.apiClient.SetUserSyncSeq(delta.LatestSeq)
				return nil
			}
			if err := c.applyUserList(newU); err != nil {
				return err
			}
			if canPrune {
				if err := c.pruneExpiredUsers(pruneUnix); err != nil {
					return err
				}
			}
			c.apiClient.SetUserSyncSeq(delta.LatestSeq)
			return nil
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if !errors.Is(err, panel.ErrUserDeltaUnsupported) {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Warn("Get user delta failed, fallback to full user list")
		}
	} else if delta != nil && delta.FullRequired {
		log.WithField("tag", c.tag).Info("User delta requires full user list")
		forceFullUserList = true
	}

	// get user info
	var newU []panel.UserInfo
	if forceFullUserList {
		newU, err = c.apiClient.GetFullUserList(ctx)
	} else {
		newU, err = c.apiClient.GetUserList(ctx)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return nil
	}

	if err := c.refreshAliveStateIfDue(ctx, true); err != nil {
		return err
	}

	// node no changed, check users
	if newU == nil {
		log.WithField("tag", c.tag).Debug("User list no change")
		return nil
	}
	if err := c.applyUserList(newU); err != nil {
		return err
	}
	if pruneUnix, canPrune := userDeltaPruneTime(delta); canPrune {
		return c.pruneExpiredUsers(pruneUnix)
	}
	return nil
}

func (c *Controller) refreshAliveStateIfDue(ctx context.Context, force bool) error {
	if !force && !c.lastAliveRefresh.IsZero() && time.Since(c.lastAliveRefresh) < c.aliveStateRefreshInterval() {
		return nil
	}
	if err := c.refreshAliveState(ctx); err != nil {
		return err
	}
	c.lastAliveRefresh = time.Now()
	return nil
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
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get alive list failed")
		return nil
	}

	useDeviceLimitByUUID := c.supportsDeviceLimitByUUID()
	var newDeviceAlive map[int]int
	if useDeviceLimitByUUID {
		newDeviceAlive, err = c.apiClient.GetUserDeviceAlive(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Error("Get device alive list failed")
			if newA != nil {
				c.aliveMap = newA
			}
			c.limiter.UpdateAliveState(newA, nil, useDeviceLimitByUUID)
			return nil
		}
		if newDeviceAlive != nil {
			c.deviceAliveMap = newDeviceAlive
		}
	}
	if newA != nil {
		c.aliveMap = newA
	}
	c.limiter.UpdateAliveState(newA, newDeviceAlive, useDeviceLimitByUUID)
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

func (c *Controller) pruneExpiredUsers(nowUnix int64) error {
	newU, changed := removeExpiredUsers(c.userList, nowUnix)
	if !changed {
		return nil
	}
	log.WithField("tag", c.tag).Info("Prune expired users from local user list")
	return c.applyUserList(newU)
}
