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
		return nil
	}

	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		c.recordPanelFailure("sync", "get node info", err)
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
		return nil
	}
	log.WithField("tag", c.tag).Debug("Node info no change")
	c.checkUpdateTask(ctx)
	c.checkStreamUnlockTask(ctx)

	if err := c.syncUserState(ctx); err != nil {
		c.recordPanelFailure("sync", "sync user state", err)
		return err
	}
	c.recordPanelSuccess("sync")
	if err := c.persistOfflineState(c.info); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Persist synchronized offline snapshot failed")
	}
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
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return fmt.Errorf("get user list: %w", err)
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
		return fmt.Errorf("get alive list: %w", err)
	}
	if newA == nil {
		return errors.New("get alive list: panel returned no data")
	}

	useDeviceLimitByUUID := c.supportsDeviceLimitByUUID()
	newDeviceAlive := make(map[int]int)
	if useDeviceLimitByUUID {
		newDeviceAlive, err = c.apiClient.GetUserDeviceAlive(ctx)
		if err != nil {
			return fmt.Errorf("get device alive list: %w", err)
		}
		if newDeviceAlive == nil {
			return errors.New("get device alive list: panel returned no data")
		}
	}
	c.aliveMap = cloneIntMap(newA)
	c.deviceAliveMap = cloneIntMap(newDeviceAlive)
	if c.limiter != nil {
		c.limiter.UpdateAliveState(newA, newDeviceAlive, useDeviceLimitByUUID)
	}
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
