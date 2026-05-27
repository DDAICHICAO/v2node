package node

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

const nodeRuntimeStatusReportTimeout = 2 * time.Second

func (c *Controller) reportUserTrafficTask(ctx context.Context) (err error) {
	var reportmin = 0
	var devicemin = 0
	if c.info.Common.BaseConfig != nil {
		reportmin = c.info.Common.BaseConfig.NodeReportMinTraffic
		devicemin = c.info.Common.BaseConfig.DeviceOnlineMinTraffic
	}
	var userTraffic []panel.UserTraffic
	var userDeviceTraffic []panel.UserDeviceTraffic
	userTraffic, userDeviceTraffic, _ = c.server.GetUserTrafficReport(c.tag, reportmin)
	trafficReported := len(userTraffic) == 0
	if len(userTraffic) > 0 {
		err = c.apiClient.ReportUserTraffic(ctx, userTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			trafficReported = true
			log.WithField("tag", c.tag).Infof("Report %d users traffic", len(userTraffic))
			//log.WithField("tag", c.tag).Debugf("User traffic: %+v", userTraffic)
		}
	}
	if trafficReported && c.supportsDeviceTrafficReport() && len(userDeviceTraffic) > 0 {
		err = c.apiClient.ReportUserDeviceTraffic(ctx, userDeviceTraffic)
		if err != nil {
			log.WithFields(log.Fields{
				"tag": c.tag,
				"err": err,
			}).Info("Report user device traffic failed")
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		} else {
			log.WithField("tag", c.tag).Infof("Report %d devices traffic", len(userDeviceTraffic))
		}
	}

	activeOnlineUIDs := make([]int, 0, len(userTraffic))
	for _, traffic := range userTraffic {
		total := traffic.Upload + traffic.Download
		if total >= int64(devicemin*1000) {
			activeOnlineUIDs = append(activeOnlineUIDs, traffic.UID)
		}
	}
	if len(activeOnlineUIDs) > 0 {
		c.limiter.RefreshOnlineUIDsFromLastSnapshot(activeOnlineUIDs)
	}

	if onlineDevice, onlineDeviceDetails, err := c.limiter.GetOnlineDeviceState(); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Info("Get online device failed")
	} else if len(*onlineDevice) > 0 {
		var result []panel.OnlineUser
		var deviceResult []panel.OnlineDevice
		var nocountUID = make(map[int]struct{})
		for _, traffic := range userTraffic {
			total := traffic.Upload + traffic.Download
			if total < int64(devicemin*1000) {
				nocountUID[traffic.UID] = struct{}{}
			}
		}
		for _, online := range *onlineDevice {
			if _, ok := nocountUID[online.UID]; !ok {
				result = append(result, online)
			}
		}
		for _, online := range *onlineDeviceDetails {
			if _, ok := nocountUID[online.UID]; !ok {
				deviceResult = append(deviceResult, online)
			}
		}
		data := make(map[int][]string)
		for _, onlineuser := range result {
			// json structure: { UID1:["ip1","ip2"],UID2:["ip3","ip4"] }
			data[onlineuser.UID] = append(data[onlineuser.UID], onlineuser.IP)
		}
		if len(data) != 0 {
			err := c.apiClient.ReportNodeOnlineUsers(ctx, &data)
			if err != nil {
				log.WithFields(log.Fields{
					"tag": c.tag,
					"err": err,
				}).Info("Report online users failed")
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
			}
		}
		if c.supportsDeviceAliveReport() && len(deviceResult) != 0 {
			deviceData := make(map[int][]panel.OnlineDeviceReportItem)
			for _, online := range deviceResult {
				if online.UUID == "" {
					continue
				}
				log.WithFields(log.Fields{
					"tag":  c.tag,
					"uid":  online.UID,
					"uuid": online.UUID,
					"ip":   online.IP,
				}).Info("SNTP online device heartbeat")
				deviceData[online.UID] = append(deviceData[online.UID], panel.OnlineDeviceReportItem{
					UUID: online.UUID,
					IP:   online.IP,
				})
			}
			if len(deviceData) != 0 {
				err := c.apiClient.ReportNodeOnlineDevices(ctx, &deviceData)
				if err != nil {
					log.WithFields(log.Fields{
						"tag": c.tag,
						"err": err,
					}).Info("Report online devices failed")
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						return err
					}
				}
			}
		}
		log.WithField("tag", c.tag).Infof("Total %d online users, %d Reported", len(*onlineDevice), len(result))
	}

	c.reportNodeRuntimeStatus(ctx)
	return nil
}

func (c *Controller) reportNodeRuntimeStatus(ctx context.Context) {
	if c.netSampler == nil {
		return
	}
	throughput, ok, err := c.netSampler.Sample()
	if err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Debug("Sample network throughput failed")
		return
	}
	if !ok || throughput == nil {
		return
	}

	reportCtx, cancel := context.WithTimeout(ctx, nodeRuntimeStatusReportTimeout)
	defer cancel()

	hostname, _ := os.Hostname()
	status := panel.NodeRuntimeStatus{
		Hostname:       strings.TrimSpace(hostname),
		Interfaces:     throughput.Interfaces,
		RxBps:          throughput.RxBps,
		TxBps:          throughput.TxBps,
		RxBytes:        throughput.RxBytes,
		TxBytes:        throughput.TxBytes,
		SampleInterval: throughput.IntervalSeconds,
		SampledAt:      throughput.CapturedAt.Unix(),
	}
	c.appendAccessAuditRuntimeStatus(&status)
	if err := c.apiClient.ReportNodeRuntimeStatus(reportCtx, status); err != nil {
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Debug("Report node runtime status failed")
	}
}

func (c *Controller) appendAccessAuditRuntimeStatus(status *panel.NodeRuntimeStatus) {
	if c == nil || status == nil || c.server == nil || c.server.Config == nil {
		return
	}
	audit := c.server.Config.AccessAuditConfig
	status.AccessAuditReported = true
	status.AccessAuditEnabled = audit.Enabled
	status.AccessAuditEndpoint = strings.TrimSpace(audit.Endpoint)
	status.AccessAuditTokenConfigured = strings.TrimSpace(audit.Token) != ""
}

func compareUserList(old, new []panel.UserInfo) (deleted, added, modified []panel.UserInfo) {
	oldMap := make(map[string]panel.UserInfo, len(old))
	for _, u := range old {
		oldMap[u.Uuid] = u
	}

	for _, u := range new {
		if o, ok := oldMap[u.Uuid]; !ok {
			added = append(added, u)
		} else {
			if o.SpeedLimit != u.SpeedLimit || o.DeviceLimit != u.DeviceLimit || !sameStringSet(o.BlockedIPs, u.BlockedIPs) {
				modified = append(modified, u)
			}
			delete(oldMap, u.Uuid)
		}
	}

	for _, o := range oldMap {
		deleted = append(deleted, o)
	}

	return deleted, added, modified
}

func (c *Controller) closeBlockedUserIPs(users []panel.UserInfo) {
	for _, user := range users {
		for _, ip := range user.BlockedIPs {
			if closed := c.server.CloseUserIP(c.tag, user.Uuid, ip); closed > 0 {
				log.WithFields(log.Fields{
					"tag":    c.tag,
					"uid":    user.Id,
					"ip":     ip,
					"closed": closed,
				}).Info("Closed blocked user connections")
			}
		}
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	seen := make(map[string]int, len(a))
	for _, item := range a {
		seen[item]++
	}
	for _, item := range b {
		seen[item]--
		if seen[item] < 0 {
			return false
		}
	}
	return true
}
