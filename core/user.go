package core

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/proxy"
	"github.com/xtls/xray-core/proxy/anytls"
	hyaccount "github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/shadowsocks"
	"github.com/xtls/xray-core/proxy/shadowsocks_2022"
	"github.com/xtls/xray-core/proxy/trojan"
	"github.com/xtls/xray-core/proxy/tuic"
	"github.com/xtls/xray-core/proxy/vless"
)

func (v *V2Core) GetUserManager(tag string) (proxy.UserManager, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handler, err := v.ihm.GetHandler(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("no such inbound tag: %s", err)
	}
	inboundInstance, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("handler %s is not implement proxy.GetInbound", tag)
	}
	userManager, ok := inboundInstance.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("handler %s is not implement proxy.UserManager", tag)
	}
	return userManager, nil
}

func (vc *V2Core) DelUsers(users []panel.UserInfo, tag string, _ *panel.NodeInfo) error {
	if server, ok := vc.eclipse[tag]; ok {
		vc.users.mapLock.Lock()
		for i := range users {
			user := format.UserTag(tag, users[i].Uuid)
			delete(vc.users.uidMap, user)
			if vc.dispatcher != nil {
				if v, ok := vc.dispatcher.Counter.Load(tag); ok {
					tc := v.(*counter.TrafficCounter)
					tc.Delete(user)
				}
				if v, ok := vc.dispatcher.LinkManagers.Load(user); ok {
					lm := v.(*dispatcher.LinkManager)
					lm.CloseAll()
					vc.dispatcher.LinkManagers.Delete(user)
				}
			}
		}
		vc.users.mapLock.Unlock()
		server.DelUsers(users)
		return nil
	}
	if server, ok := vc.mieru[tag]; ok {
		vc.users.mapLock.Lock()
		for i := range users {
			user := format.UserTag(tag, users[i].Uuid)
			delete(vc.users.uidMap, user)
			if vc.dispatcher != nil {
				if v, ok := vc.dispatcher.Counter.Load(tag); ok {
					tc := v.(*counter.TrafficCounter)
					tc.Delete(user)
				}
				if v, ok := vc.dispatcher.LinkManagers.Load(user); ok {
					lm := v.(*dispatcher.LinkManager)
					lm.CloseAll()
					vc.dispatcher.LinkManagers.Delete(user)
				}
			}
		}
		vc.users.mapLock.Unlock()
		return server.DelUsers(users)
	}
	userManager, err := vc.GetUserManager(tag)
	if err != nil {
		return fmt.Errorf("get user manager error: %s", err)
	}
	var user string
	vc.users.mapLock.Lock()
	defer vc.users.mapLock.Unlock()
	for i := range users {
		user = format.UserTag(tag, users[i].Uuid)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = userManager.RemoveUser(ctx, user)
		cancel()
		if err != nil {
			return err
		}
		delete(vc.users.uidMap, user)
		if v, ok := vc.dispatcher.Counter.Load(tag); ok {
			tc := v.(*counter.TrafficCounter)
			tc.Delete(user)
		}
		if v, ok := vc.dispatcher.LinkManagers.Load(user); ok {
			lm := v.(*dispatcher.LinkManager)
			lm.CloseAll()
			vc.dispatcher.LinkManagers.Delete(user)
		}
	}
	return nil
}

func (vc *V2Core) CloseUserIP(tag string, uuid string, ip string) int {
	if vc.dispatcher == nil {
		return 0
	}
	user := format.UserTag(tag, uuid)
	if v, ok := vc.dispatcher.LinkManagers.Load(user); ok {
		lm := v.(*dispatcher.LinkManager)
		return lm.CloseByIP(strings.TrimPrefix(strings.TrimSpace(ip), "::ffff:"))
	}
	return 0
}

func (vc *V2Core) GetUserTrafficSlice(tag string, mintraffic int) ([]panel.UserTraffic, error) {
	userTraffic, _, err := vc.GetUserTrafficReport(tag, mintraffic)
	return userTraffic, err
}

func (vc *V2Core) GetUserTrafficReport(tag string, mintraffic int) ([]panel.UserTraffic, []panel.UserDeviceTraffic, error) {
	report := newTrafficCounterReport()
	vc.users.mapLock.RLock()
	defer vc.users.mapLock.RUnlock()
	if server, ok := vc.eclipse[tag]; ok {
		collectTrafficCounters(server.counter, vc.users.uidMap, report)
		if vc.dispatcher != nil {
			if v, ok := vc.dispatcher.Counter.Load(tag); ok {
				c := v.(*counter.TrafficCounter)
				collectTrafficCounters(c, vc.users.uidMap, report)
			}
		}
		userTraffic, deviceTraffic := flushTrafficCounters(report, mintraffic)
		return userTraffic, deviceTraffic, nil
	}
	if server, ok := vc.mieru[tag]; ok {
		collectTrafficCounters(server.counter, vc.users.uidMap, report)
		if vc.dispatcher != nil {
			if v, ok := vc.dispatcher.Counter.Load(tag); ok {
				c := v.(*counter.TrafficCounter)
				collectTrafficCounters(c, vc.users.uidMap, report)
			}
		}
		userTraffic, deviceTraffic := flushTrafficCounters(report, mintraffic)
		return userTraffic, deviceTraffic, nil
	}
	if v, ok := vc.dispatcher.Counter.Load(tag); ok {
		c := v.(*counter.TrafficCounter)
		collectTrafficCounters(c, vc.users.uidMap, report)
		userTraffic, deviceTraffic := flushTrafficCounters(report, mintraffic)
		if len(userTraffic) == 0 && len(deviceTraffic) == 0 {
			return nil, nil, nil
		}
		return userTraffic, deviceTraffic, nil
	}
	return nil, nil, nil
}

func (v *V2Core) AddUsers(p *AddUsersParams) (added int, err error) {
	if server, ok := v.eclipse[p.Tag]; ok {
		server.AddUsers(p.Users)
		v.commitUserUIDs(p.Tag, p.Users)
		return len(p.Users), nil
	}
	if server, ok := v.mieru[p.Tag]; ok {
		if err := server.AddUsers(p.Users); err != nil {
			return 0, err
		}
		v.commitUserUIDs(p.Tag, p.Users)
		return len(p.Users), nil
	}
	var users []*protocol.User
	switch p.NodeInfo.Type {
	case "vmess":
		users = buildVmessUsers(p.Tag, p.Users)
	case "vless":
		users = buildVlessUsers(p.Tag, p.Users, p.Common.Flow)
	case "trojan":
		users = buildTrojanUsers(p.Tag, p.Users)
	case "shadowsocks":
		users = buildSSUsers(p.Tag,
			p.Users,
			p.Common.Cipher,
			p.Common.ServerKey)
	case "hysteria2":
		users = buildHysteria2Users(p.Tag, p.Users)
	case "tuic":
		users = buildTuicUsers(p.Tag, p.Users)
	case "anytls":
		users = buildAnyTLSUsers(p.Tag, p.Users)
	default:
		return 0, fmt.Errorf("unsupported node type: %s", p.NodeInfo.Type)
	}
	man, err := v.GetUserManager(p.Tag)
	if err != nil {
		return 0, fmt.Errorf("get user manager error: %s", err)
	}
	addedEmails := make([]string, 0, len(users))
	for _, u := range users {
		mUser, err := u.ToMemoryUser()
		if err != nil {
			rollbackAddedUsers(man, addedEmails)
			return 0, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err = man.AddUser(ctx, mUser)
		cancel()
		if err != nil {
			rollbackAddedUsers(man, addedEmails)
			return 0, err
		}
		addedEmails = append(addedEmails, u.Email)
	}
	v.commitUserUIDs(p.Tag, p.Users)
	return len(users), nil
}

func (v *V2Core) commitUserUIDs(tag string, users []panel.UserInfo) {
	v.users.mapLock.Lock()
	defer v.users.mapLock.Unlock()
	for i := range users {
		v.users.uidMap[format.UserTag(tag, users[i].Uuid)] = users[i].Id
	}
}

func rollbackAddedUsers(man proxy.UserManager, emails []string) {
	for _, email := range emails {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = man.RemoveUser(ctx, email)
		cancel()
	}
}

type trafficCounterReport struct {
	trafficByUID  map[int]*panel.UserTraffic
	countersByUID map[int][]*counter.TrafficStorage
	deviceByKey   map[string]*panel.UserDeviceTraffic
	uidOrder      []int
	deviceOrder   []string
}

func newTrafficCounterReport() *trafficCounterReport {
	return &trafficCounterReport{
		trafficByUID:  make(map[int]*panel.UserTraffic),
		countersByUID: make(map[int][]*counter.TrafficStorage),
		deviceByKey:   make(map[string]*panel.UserDeviceTraffic),
		uidOrder:      make([]int, 0),
		deviceOrder:   make([]string, 0),
	}
}

func collectTrafficCounters(c *counter.TrafficCounter, uidMap map[string]int, report *trafficCounterReport) {
	c.Counters.Range(func(key, value interface{}) bool {
		email := key.(string)
		traffic := value.(*counter.TrafficStorage)
		up := traffic.UpCounter.Load()
		down := traffic.DownCounter.Load()
		uid := uidMap[email]
		if uid == 0 {
			c.Delete(email)
			return true
		}
		if up+down > 0 {
			if existing, ok := report.trafficByUID[uid]; ok {
				existing.Upload += up
				existing.Download += down
			} else {
				report.trafficByUID[uid] = &panel.UserTraffic{
					UID:      uid,
					Upload:   up,
					Download: down,
				}
				report.uidOrder = append(report.uidOrder, uid)
			}
			uuid := extractUUIDFromUserTag(email)
			if uuid != "" {
				deviceKey := fmt.Sprintf("%d|%s", uid, uuid)
				if existing, ok := report.deviceByKey[deviceKey]; ok {
					existing.Upload += up
					existing.Download += down
				} else {
					report.deviceByKey[deviceKey] = &panel.UserDeviceTraffic{
						UID:      uid,
						UUID:     uuid,
						Upload:   up,
						Download: down,
					}
					report.deviceOrder = append(report.deviceOrder, deviceKey)
				}
			}
			report.countersByUID[uid] = append(report.countersByUID[uid], traffic)
		}
		return true
	})
}

func flushTrafficCounters(report *trafficCounterReport, mintraffic int) ([]panel.UserTraffic, []panel.UserDeviceTraffic) {
	trafficSlice := make([]panel.UserTraffic, 0)
	deviceTrafficSlice := make([]panel.UserDeviceTraffic, 0)
	reportedUIDs := make(map[int]struct{})
	minBytes := int64(mintraffic * 1000)
	for _, uid := range report.uidOrder {
		traffic := report.trafficByUID[uid]
		if traffic.Upload+traffic.Download <= minBytes {
			continue
		}
		for _, storage := range report.countersByUID[uid] {
			storage.UpCounter.Store(0)
			storage.DownCounter.Store(0)
		}
		trafficSlice = append(trafficSlice, *traffic)
		reportedUIDs[uid] = struct{}{}
	}
	for _, key := range report.deviceOrder {
		traffic := report.deviceByKey[key]
		if _, ok := reportedUIDs[traffic.UID]; !ok {
			continue
		}
		deviceTrafficSlice = append(deviceTrafficSlice, *traffic)
	}
	return trafficSlice, deviceTrafficSlice
}

func extractUUIDFromUserTag(userTag string) string {
	idx := strings.LastIndex(userTag, "|")
	if idx < 0 || idx+1 >= len(userTag) {
		return userTag
	}
	return userTag[idx+1:]
}

func buildVmessUsers(tag string, userInfo []panel.UserInfo) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i, user := range userInfo {
		users[i] = buildVmessUser(tag, &user)
	}
	return users
}

func buildVmessUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	vmessAccount := &conf.VMessAccount{
		ID:       userInfo.Uuid,
		Security: "auto",
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(vmessAccount.Build()),
	}
}

func buildVlessUsers(tag string, userInfo []panel.UserInfo, flow string) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildVlessUser(tag, &(userInfo)[i], flow)
	}
	return users
}

func buildVlessUser(tag string, userInfo *panel.UserInfo, flow string) (user *protocol.User) {
	vlessAccount := &vless.Account{
		Id: userInfo.Uuid,
	}
	vlessAccount.Flow = flow
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(vlessAccount),
	}
}

func buildTrojanUsers(tag string, userInfo []panel.UserInfo) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildTrojanUser(tag, &(userInfo)[i])
	}
	return users
}

func buildTrojanUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	trojanAccount := &trojan.Account{
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(trojanAccount),
	}
}

func buildSSUsers(tag string, userInfo []panel.UserInfo, cypher string, serverKey string) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildSSUser(tag, &userInfo[i], cypher, serverKey)
	}
	return users
}

func buildSSUser(tag string, userInfo *panel.UserInfo, cypher string, serverKey string) (user *protocol.User) {
	if serverKey == "" {
		ssAccount := &shadowsocks.Account{
			Password:   userInfo.Uuid,
			CipherType: getCipherFromString(cypher),
		}
		return &protocol.User{
			Level:   0,
			Email:   format.UserTag(tag, userInfo.Uuid),
			Account: serial.ToTypedMessage(ssAccount),
		}
	} else {
		var keyLength int
		switch cypher {
		case "2022-blake3-aes-128-gcm":
			keyLength = 16
		case "2022-blake3-aes-256-gcm":
			keyLength = 32
		case "2022-blake3-chacha20-poly1305":
			keyLength = 32
		}
		ssAccount := &shadowsocks_2022.Account{
			Key: base64.StdEncoding.EncodeToString([]byte(userInfo.Uuid[:keyLength])),
		}
		return &protocol.User{
			Level:   0,
			Email:   format.UserTag(tag, userInfo.Uuid),
			Account: serial.ToTypedMessage(ssAccount),
		}
	}
}

func getCipherFromString(c string) shadowsocks.CipherType {
	switch strings.ToLower(c) {
	case "aes-128-gcm", "aead_aes_128_gcm":
		return shadowsocks.CipherType_AES_128_GCM
	case "aes-256-gcm", "aead_aes_256_gcm":
		return shadowsocks.CipherType_AES_256_GCM
	case "chacha20-poly1305", "aead_chacha20_poly1305", "chacha20-ietf-poly1305":
		return shadowsocks.CipherType_CHACHA20_POLY1305
	case "none", "plain":
		return shadowsocks.CipherType_NONE
	default:
		return shadowsocks.CipherType_UNKNOWN
	}
}

func buildHysteria2Users(tag string, userInfo []panel.UserInfo) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildHysteria2User(tag, &userInfo[i])
	}
	return users
}

func buildHysteria2User(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	hysteria2Account := &hyaccount.Account{
		Auth: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(hysteria2Account),
	}
}

func buildTuicUsers(tag string, userInfo []panel.UserInfo) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildTuicUser(tag, &userInfo[i])
	}
	return users
}

func buildTuicUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	tuicAccount := &tuic.Account{
		Uuid:     userInfo.Uuid,
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(tuicAccount),
	}
}

func buildAnyTLSUsers(tag string, userInfo []panel.UserInfo) (users []*protocol.User) {
	users = make([]*protocol.User, len(userInfo))
	for i := range userInfo {
		users[i] = buildAnyTLSUser(tag, &userInfo[i])
	}
	return users
}

func buildAnyTLSUser(tag string, userInfo *panel.UserInfo) (user *protocol.User) {
	anyTLSAccount := &anytls.Account{
		Password: userInfo.Uuid,
	}
	return &protocol.User{
		Level:   0,
		Email:   format.UserTag(tag, userInfo.Uuid),
		Account: serial.ToTypedMessage(anyTLSAccount),
	}
}
