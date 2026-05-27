package limiter

import (
	"errors"
	"strings"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/rate"
)

var limitLock sync.RWMutex
var limiter map[string]*Limiter

func Init() {
	limiter = map[string]*Limiter{}
}

type Limiter struct {
	Nodetype               string         // Node type, e.g. "v2ray", "trojan", "shadowsocks"
	SpeedLimit             int            // Node speed limit in Mbps
	UserOnlineIP           *sync.Map      // Key: TagUUID, value: {Key: Ip, value: Uid}
	OldUserOnline          *sync.Map      // Key: Ip, value: Uid
	OldUserOnlineDevice    *sync.Map      // Key: TagUUID, value: Uid
	OldUserOnlineDeviceIPs *sync.Map      // Key: TagUUID, value: []Ip from last reported snapshot
	UUIDtoUID              map[string]int // Key: UUID, value: Uid
	stateMu                sync.RWMutex
	UserLimitInfo          *sync.Map   // Key: TagUUID value: UserLimitInfo
	SpeedLimiter           *sync.Map   // key: TagUUID, value: *DynamicBucket
	AliveList              map[int]int // Key: Uid, value: alive_ip
	DeviceAliveList        map[int]int // Key: Uid, value: alive device UUID count
	UseDeviceLimitByUUID   bool
}

type UserLimitInfo struct {
	UID               int
	SpeedLimit        int
	DeviceLimit       int
	DynamicSpeedLimit int
	ExpireTime        int64
	OverLimit         bool
	BlockedIPs        map[string]struct{}
}

type LimitRejectReason string

const (
	LimitRejectReasonNone                LimitRejectReason = ""
	LimitRejectReasonUserNotFound        LimitRejectReason = "user_not_found"
	LimitRejectReasonBlockedIP           LimitRejectReason = "blocked_ip"
	LimitRejectReasonDeviceLimitExceeded LimitRejectReason = "device_limit_exceeded"
)

type LimitRejectInfo struct {
	Reason               LimitRejectReason
	UID                  int
	IP                   string
	DeviceLimit          int
	AliveCount           int
	PendingDeviceCount   int
	CachedDeviceOverlap  int
	EffectiveDeviceCount int
	UseDeviceLimitByUUID bool
}

func (r LimitRejectReason) String() string {
	if r == LimitRejectReasonNone {
		return "none"
	}
	return string(r)
}

func AddLimiter(nodetype string, tag string, users []panel.UserInfo, aliveList map[int]int, deviceAliveList map[int]int, useDeviceLimitByUUID bool) *Limiter {
	l := &Limiter{
		Nodetype:               nodetype,
		UserOnlineIP:           new(sync.Map),
		UserLimitInfo:          new(sync.Map),
		SpeedLimiter:           new(sync.Map),
		AliveList:              cloneIntMap(aliveList),
		DeviceAliveList:        cloneIntMap(deviceAliveList),
		UseDeviceLimitByUUID:   useDeviceLimitByUUID,
		OldUserOnline:          new(sync.Map),
		OldUserOnlineDevice:    new(sync.Map),
		OldUserOnlineDeviceIPs: new(sync.Map),
	}
	uuidmap := make(map[string]int)
	for i := range users {
		uuidmap[users[i].Uuid] = users[i].Id
		userLimit := &UserLimitInfo{}
		userLimit.UID = users[i].Id
		if users[i].SpeedLimit != 0 {
			userLimit.SpeedLimit = users[i].SpeedLimit
		}
		if users[i].DeviceLimit != 0 {
			userLimit.DeviceLimit = users[i].DeviceLimit
		}
		userLimit.BlockedIPs = makeBlockedIPSet(users[i].BlockedIPs)
		userLimit.OverLimit = false
		l.UserLimitInfo.Store(format.UserTag(tag, users[i].Uuid), userLimit)
	}
	l.UUIDtoUID = uuidmap
	limitLock.Lock()
	limiter[tag] = l
	limitLock.Unlock()
	return l
}

func GetLimiter(tag string) (info *Limiter, err error) {
	limitLock.RLock()
	info, ok := limiter[tag]
	limitLock.RUnlock()
	if !ok {
		return nil, errors.New("not found")
	}
	return info, nil
}

func DeleteLimiter(tag string) {
	limitLock.Lock()
	delete(limiter, tag)
	limitLock.Unlock()
}

func (l *Limiter) UpdateUser(tag string, added []panel.UserInfo, deleted []panel.UserInfo, modified []panel.UserInfo) {
	for i := range deleted {
		l.UserLimitInfo.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.UserOnlineIP.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.OldUserOnlineDevice.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.OldUserOnlineDeviceIPs.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.SpeedLimiter.Delete(format.UserTag(tag, deleted[i].Uuid))
		l.stateMu.Lock()
		delete(l.UUIDtoUID, deleted[i].Uuid)
		l.stateMu.Unlock()
	}
	for i := range modified {
		userLimit := userLimitInfoFromUser(modified[i])
		if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, modified[i].Uuid)); ok {
			old := v.(*UserLimitInfo)
			userLimit.DynamicSpeedLimit = old.DynamicSpeedLimit
			userLimit.ExpireTime = old.ExpireTime
			userLimit.OverLimit = old.OverLimit
		}
		l.UserLimitInfo.Store(format.UserTag(tag, modified[i].Uuid), userLimit)
		limit := int64(determineSpeedLimit(l.SpeedLimit, modified[i].SpeedLimit)) * 1000000 / 8
		if limit > 0 {
			if v, ok := l.SpeedLimiter.Load(format.UserTag(tag, modified[i].Uuid)); ok {
				d := v.(*rate.DynamicBucket)
				d.Update(limit)
			} else {
				d := rate.NewDynamicBucket(limit)
				l.SpeedLimiter.Store(format.UserTag(tag, modified[i].Uuid), d)
			}
		} else {
			if v, ok := l.SpeedLimiter.Load(format.UserTag(tag, modified[i].Uuid)); ok {
				v.(*rate.DynamicBucket).Disable()
				l.SpeedLimiter.Delete(format.UserTag(tag, modified[i].Uuid))
			}
		}
	}
	for i := range added {
		userLimit := userLimitInfoFromUser(added[i])
		l.UserLimitInfo.Store(format.UserTag(tag, added[i].Uuid), userLimit)
		l.stateMu.Lock()
		l.UUIDtoUID[added[i].Uuid] = added[i].Id
		l.stateMu.Unlock()
	}
}

func (l *Limiter) UpdateDynamicSpeedLimit(tag, uuid string, limit int, expire time.Time) error {
	if v, ok := l.UserLimitInfo.Load(format.UserTag(tag, uuid)); ok {
		old := v.(*UserLimitInfo)
		info := *old
		info.DynamicSpeedLimit = limit
		info.ExpireTime = expire.Unix()
		l.UserLimitInfo.Store(format.UserTag(tag, uuid), &info)
	} else {
		return errors.New("not found")
	}
	return nil
}

func (l *Limiter) UpdateAliveState(aliveList map[int]int, deviceAliveList map[int]int, useDeviceLimitByUUID bool) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	if aliveList != nil {
		l.AliveList = cloneIntMap(aliveList)
	}
	if deviceAliveList != nil {
		l.DeviceAliveList = cloneIntMap(deviceAliveList)
	}
	l.UseDeviceLimitByUUID = useDeviceLimitByUUID
}

func (l *Limiter) CheckLimit(taguuid string, ip string, noUDPsource bool) (DynamicBucket *rate.DynamicBucket, Reject bool, RejectInfo LimitRejectInfo) {
	// check if ipv4 mapped ipv6
	ip = normalizeIP(ip)
	useDeviceLimitByUUID := l.useDeviceLimitByUUID()

	// check and gen speed limit Bucket
	nodeLimit := l.SpeedLimit
	userLimit := 0
	deviceLimit := 0
	var uid int
	if v, ok := l.UserLimitInfo.Load(taguuid); ok {
		u := v.(*UserLimitInfo)
		deviceLimit = u.DeviceLimit
		uid = u.UID
		if _, blocked := u.BlockedIPs[ip]; blocked {
			return nil, true, LimitRejectInfo{
				Reason:               LimitRejectReasonBlockedIP,
				UID:                  uid,
				IP:                   ip,
				DeviceLimit:          deviceLimit,
				UseDeviceLimitByUUID: useDeviceLimitByUUID,
			}
		}
		if u.ExpireTime < time.Now().Unix() && u.ExpireTime != 0 {
			if u.SpeedLimit != 0 {
				userLimit = u.SpeedLimit
				next := *u
				next.DynamicSpeedLimit = 0
				next.ExpireTime = 0
				l.UserLimitInfo.Store(taguuid, &next)
			} else {
				l.UserLimitInfo.Delete(taguuid)
			}
		} else {
			userLimit = determineSpeedLimit(u.SpeedLimit, u.DynamicSpeedLimit)
		}
	} else {
		return nil, true, LimitRejectInfo{
			Reason:               LimitRejectReasonUserNotFound,
			IP:                   ip,
			UseDeviceLimitByUUID: useDeviceLimitByUUID,
		}
	}
	if noUDPsource || l.Nodetype == "anytls" || l.Nodetype == "hysteria2" || l.Nodetype == "tuic" || l.Nodetype == "mieru" {
		// Store online user for device limit
		newipMap := new(sync.Map)
		newipMap.Store(ip, uid)
		aliveCount := l.aliveCount(uid, useDeviceLimitByUUID)
		if useDeviceLimitByUUID {
			if v, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
				oldipMap := v.(*sync.Map)
				oldipMap.Store(ip, uid)
			} else if v, ok := l.OldUserOnlineDevice.Load(taguuid); ok {
				if v.(int) == uid {
					l.OldUserOnlineDevice.Delete(taguuid)
				}
			} else if deviceLimit > 0 {
				pendingDeviceCount := l.countPendingDeviceUuids(uid)
				cachedDeviceOverlap := minInt(aliveCount, pendingDeviceCount)
				effectiveDeviceCount := aliveCount + pendingDeviceCount - cachedDeviceOverlap
				if deviceLimit < effectiveDeviceCount {
					l.UserOnlineIP.Delete(taguuid)
					return nil, true, LimitRejectInfo{
						Reason:               LimitRejectReasonDeviceLimitExceeded,
						UID:                  uid,
						IP:                   ip,
						DeviceLimit:          deviceLimit,
						AliveCount:           aliveCount,
						PendingDeviceCount:   pendingDeviceCount,
						CachedDeviceOverlap:  cachedDeviceOverlap,
						EffectiveDeviceCount: effectiveDeviceCount,
						UseDeviceLimitByUUID: useDeviceLimitByUUID,
					}
				}
			}
		} else {
			// If any device is online
			if v, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
				oldipMap := v.(*sync.Map)
				// If this is a new ip
				if _, loaded := oldipMap.LoadOrStore(ip, uid); !loaded {
					if v, loaded := l.OldUserOnline.Load(ip); loaded {
						if v.(int) == uid {
							l.OldUserOnline.Delete(ip)
						}
					} else if deviceLimit > 0 {
						if deviceLimit <= aliveCount {
							oldipMap.Delete(ip)
							return nil, true, LimitRejectInfo{
								Reason:               LimitRejectReasonDeviceLimitExceeded,
								UID:                  uid,
								IP:                   ip,
								DeviceLimit:          deviceLimit,
								AliveCount:           aliveCount,
								UseDeviceLimitByUUID: useDeviceLimitByUUID,
							}
						}
					}
				}
			} else if v, ok := l.OldUserOnline.Load(ip); ok {
				if v.(int) == uid {
					l.OldUserOnline.Delete(ip)
				}
			} else {
				if deviceLimit > 0 {
					if deviceLimit <= aliveCount {
						l.UserOnlineIP.Delete(taguuid)
						return nil, true, LimitRejectInfo{
							Reason:               LimitRejectReasonDeviceLimitExceeded,
							UID:                  uid,
							IP:                   ip,
							DeviceLimit:          deviceLimit,
							AliveCount:           aliveCount,
							UseDeviceLimitByUUID: useDeviceLimitByUUID,
						}
					}
				}
			}
		}
	}

	limit := int64(determineSpeedLimit(nodeLimit, userLimit)) * 1000000 / 8 // If you need the Speed limit
	if limit > 0 {
		if v, ok := l.SpeedLimiter.Load(taguuid); ok {
			return v.(*rate.DynamicBucket), false, LimitRejectInfo{}
		} else {
			d := rate.NewDynamicBucket(limit)
			l.SpeedLimiter.Store(taguuid, d)
			return d, false, LimitRejectInfo{}
		}
	} else {
		if v, ok := l.SpeedLimiter.Load(taguuid); ok {
			v.(*rate.DynamicBucket).Disable()
			l.SpeedLimiter.Delete(taguuid)
		}
		return nil, false, LimitRejectInfo{}
	}
}

func (l *Limiter) MarkOnline(taguuid string, ip string) bool {
	ip = normalizeIP(ip)
	if ip == "" {
		return false
	}

	v, ok := l.UserLimitInfo.Load(taguuid)
	if !ok {
		return false
	}
	info := v.(*UserLimitInfo)
	if _, blocked := info.BlockedIPs[ip]; blocked {
		return false
	}

	newipMap := new(sync.Map)
	newipMap.Store(ip, info.UID)
	if existing, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
		existing.(*sync.Map).Store(ip, info.UID)
	}
	if oldUid, ok := l.OldUserOnline.Load(ip); ok {
		if oldUidInt, ok := oldUid.(int); ok && oldUidInt == info.UID {
			l.OldUserOnline.Delete(ip)
		}
	}
	if oldUid, ok := l.OldUserOnlineDevice.Load(taguuid); ok {
		if oldUidInt, ok := oldUid.(int); ok && oldUidInt == info.UID {
			l.OldUserOnlineDevice.Delete(taguuid)
		}
	}
	return true
}

func (l *Limiter) RefreshOnlineUIDsFromLastSnapshot(uids []int) int {
	if len(uids) == 0 {
		return 0
	}

	uidSet := make(map[int]struct{}, len(uids))
	for _, uid := range uids {
		if uid > 0 {
			uidSet[uid] = struct{}{}
		}
	}
	if len(uidSet) == 0 {
		return 0
	}

	refreshed := 0
	l.OldUserOnlineDeviceIPs.Range(func(key, value interface{}) bool {
		taguuid, ok := key.(string)
		if !ok || taguuid == "" {
			return true
		}
		oldUID, ok := l.OldUserOnlineDevice.Load(taguuid)
		if !ok {
			return true
		}
		uid, ok := oldUID.(int)
		if !ok {
			return true
		}
		if _, active := uidSet[uid]; !active {
			return true
		}

		ips, ok := value.([]string)
		if !ok || len(ips) == 0 {
			return true
		}
		newipMap := new(sync.Map)
		hasIP := false
		for _, ip := range ips {
			ip = normalizeIP(ip)
			if ip == "" || l.isIPBlocked(taguuid, ip) {
				continue
			}
			newipMap.Store(ip, uid)
			hasIP = true
		}
		if !hasIP {
			return true
		}

		if existing, loaded := l.UserOnlineIP.LoadOrStore(taguuid, newipMap); loaded {
			if existingMap, ok := existing.(*sync.Map); ok {
				newipMap.Range(func(ip, uid interface{}) bool {
					existingMap.Store(ip, uid)
					return true
				})
			}
		}
		refreshed++
		return true
	})

	return refreshed
}

func (l *Limiter) GetOnlineDeviceState() (*[]panel.OnlineUser, *[]panel.OnlineDevice, error) {
	var onlineUser []panel.OnlineUser
	var onlineDevice []panel.OnlineDevice
	oldUserOnline := new(sync.Map)
	oldUserOnlineDevice := new(sync.Map)
	oldUserOnlineDeviceIPs := new(sync.Map)
	l.UserOnlineIP.Range(func(key, value interface{}) bool {
		taguuid := key.(string)
		ipMap := value.(*sync.Map)
		deviceUUID := extractUUIDFromTagUUID(taguuid)
		deviceSeen := false
		deviceIPs := make([]string, 0)
		ipMap.Range(func(key, value interface{}) bool {
			uid := value.(int)
			ip := normalizeIP(key.(string))
			if l.isIPBlocked(taguuid, ip) {
				return true
			}
			oldUserOnline.Store(ip, uid)
			deviceIPs = append(deviceIPs, ip)
			if !deviceSeen {
				oldUserOnlineDevice.Store(taguuid, uid)
				deviceSeen = true
			}
			onlineUser = append(onlineUser, panel.OnlineUser{UID: uid, IP: ip})
			onlineDevice = append(onlineDevice, panel.OnlineDevice{UID: uid, UUID: deviceUUID, IP: ip})
			return true
		})
		if len(deviceIPs) > 0 {
			oldUserOnlineDeviceIPs.Store(taguuid, deviceIPs)
		}
		l.UserOnlineIP.Delete(taguuid) // Reset online device
		return true
	})
	l.OldUserOnline = oldUserOnline
	l.OldUserOnlineDevice = oldUserOnlineDevice
	l.OldUserOnlineDeviceIPs = oldUserOnlineDeviceIPs

	return &onlineUser, &onlineDevice, nil
}

func (l *Limiter) GetOnlineDevice() (*[]panel.OnlineUser, error) {
	onlineUser, _, err := l.GetOnlineDeviceState()
	return onlineUser, err
}

type UserIpList struct {
	Uid    int      `json:"Uid"`
	IpList []string `json:"Ips"`
}

func makeBlockedIPSet(ips []string) map[string]struct{} {
	if len(ips) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		ip = normalizeIP(ip)
		if ip != "" {
			set[ip] = struct{}{}
		}
	}
	return set
}

func userLimitInfoFromUser(user panel.UserInfo) *UserLimitInfo {
	info := &UserLimitInfo{
		UID:        user.Id,
		BlockedIPs: makeBlockedIPSet(user.BlockedIPs),
	}
	if user.SpeedLimit != 0 {
		info.SpeedLimit = user.SpeedLimit
	}
	if user.DeviceLimit != 0 {
		info.DeviceLimit = user.DeviceLimit
	}
	return info
}

func cloneIntMap(input map[int]int) map[int]int {
	output := make(map[int]int, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func (l *Limiter) useDeviceLimitByUUID() bool {
	l.stateMu.RLock()
	defer l.stateMu.RUnlock()
	return l.UseDeviceLimitByUUID
}

func (l *Limiter) aliveCount(uid int, useDeviceLimitByUUID bool) int {
	l.stateMu.RLock()
	defer l.stateMu.RUnlock()
	if useDeviceLimitByUUID {
		return l.DeviceAliveList[uid]
	}
	return l.AliveList[uid]
}

func normalizeIP(ip string) string {
	return strings.TrimSpace(strings.TrimPrefix(ip, "::ffff:"))
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func extractUUIDFromTagUUID(taguuid string) string {
	idx := strings.LastIndex(taguuid, "|")
	if idx < 0 || idx+1 >= len(taguuid) {
		return taguuid
	}
	return taguuid[idx+1:]
}

func (l *Limiter) countPendingDeviceUuids(uid int) int {
	count := 0
	l.UserOnlineIP.Range(func(key, value interface{}) bool {
		taguuid, ok := key.(string)
		if !ok {
			return true
		}
		if oldUid, ok := l.OldUserOnlineDevice.Load(taguuid); ok {
			if oldUidInt, ok := oldUid.(int); ok && oldUidInt == uid {
				return true
			}
		}
		ipMap, ok := value.(*sync.Map)
		if !ok {
			return true
		}
		found := false
		ipMap.Range(func(_, value interface{}) bool {
			if value.(int) == uid {
				found = true
				return false
			}
			return true
		})
		if found {
			count++
		}
		return true
	})
	return count
}

func (l *Limiter) isIPBlocked(taguuid string, ip string) bool {
	if v, ok := l.UserLimitInfo.Load(taguuid); ok {
		u := v.(*UserLimitInfo)
		_, blocked := u.BlockedIPs[normalizeIP(ip)]
		return blocked
	}
	return false
}
