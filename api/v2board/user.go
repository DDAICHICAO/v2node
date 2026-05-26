package panel

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/vmihailenco/msgpack/v5"
)

type OnlineUser struct {
	UID int
	IP  string
}

type OnlineDevice struct {
	UID  int
	UUID string
	IP   string
}

type OnlineDeviceReportItem struct {
	UUID string `json:"uuid"`
	IP   string `json:"ip"`
}

type UserInfo struct {
	Id          int      `json:"id" msgpack:"id"`
	Uuid        string   `json:"uuid" msgpack:"uuid"`
	SpeedLimit  int      `json:"speed_limit" msgpack:"speed_limit"`
	DeviceLimit int      `json:"device_limit" msgpack:"device_limit"`
	ExpiredAt   int64    `json:"expired_at" msgpack:"expired_at"`
	BlockedIPs  []string `json:"blocked_ips" msgpack:"blocked_ips"`
}

type UserListBody struct {
	Users []UserInfo `json:"users" msgpack:"users"`
}

const (
	UserDeltaActionUpsert = "user_upsert"
	UserDeltaActionDelete = "user_delete"
)

var ErrUserDeltaUnsupported = errors.New("user delta endpoint unsupported")

type UserDeltaResponse struct {
	Data UserDeltaData `json:"data"`
}

type UserDeltaData struct {
	FullRequired bool             `json:"full_required"`
	LatestSeq    int64            `json:"latest_seq"`
	ServerTime   int64            `json:"server_time"`
	Events       []UserDeltaEvent `json:"events"`
}

type UserDeltaEvent struct {
	Seq       int64      `json:"seq"`
	Action    string     `json:"action"`
	UserID    int        `json:"user_id"`
	Users     []UserInfo `json:"users"`
	CreatedAt int64      `json:"created_at"`
}

type AliveMap struct {
	Alive map[int]int `json:"alive"`
}

type DeviceAliveMap struct {
	AliveDevices map[int]int `json:"alive_devices"`
}

// GetUserList will pull user from v2board
func (c *Client) GetUserList(ctx context.Context) ([]UserInfo, error) {
	const path = "/api/v1/server/UniProxy/user"
	r, err := c.client.R().
		SetContext(ctx).
		SetHeader("If-None-Match", c.userEtag).
		SetHeader("X-Response-Format", "msgpack").
		SetDoNotParseResponse(true).
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil || r.RawResponse == nil {
		return nil, fmt.Errorf("received nil response or raw response")
	}
	defer r.RawResponse.Body.Close()

	if r.StatusCode() == 304 {
		c.updateUserSyncSeqFromHeader(r.Header().Get("X-User-Sync-Seq"))
		return nil, nil
	}
	userlist := &UserListBody{}
	if strings.Contains(r.Header().Get("Content-Type"), "application/x-msgpack") {
		decoder := msgpack.NewDecoder(r.RawResponse.Body)
		if err := decoder.Decode(userlist); err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
	} else {
		dec := jsontext.NewDecoder(r.RawResponse.Body)
		for {
			tok, err := dec.ReadToken()
			if err != nil {
				return nil, fmt.Errorf("decode user list error: %w", err)
			}
			if tok.Kind() == '"' && tok.String() == "users" {
				break
			}
		}
		tok, err := dec.ReadToken()
		if err != nil {
			return nil, fmt.Errorf("decode user list error: %w", err)
		}
		if tok.Kind() != '[' {
			return nil, fmt.Errorf(`decode user list error: expected "users" array`)
		}
		for dec.PeekKind() != ']' {
			val, err := dec.ReadValue()
			if err != nil {
				return nil, fmt.Errorf("decode user list error: read user object: %w", err)
			}
			var u UserInfo
			if err := json.Unmarshal(val, &u); err != nil {
				return nil, fmt.Errorf("decode user list error: unmarshal user error: %w", err)
			}
			userlist.Users = append(userlist.Users, u)
		}
	}
	c.userEtag = r.Header().Get("ETag")
	c.updateUserSyncSeqFromHeader(r.Header().Get("X-User-Sync-Seq"))
	return userlist.Users, nil
}

func (c *Client) GetUserDelta(ctx context.Context) (*UserDeltaData, error) {
	const path = "/api/v2/server/user-delta"
	r, err := c.client.R().
		SetContext(ctx).
		SetQueryParam("since_seq", strconv.FormatInt(c.userSyncSeq, 10)).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("received nil user delta response")
	}
	if r.StatusCode() == 404 || r.StatusCode() == 405 {
		return nil, ErrUserDeltaUnsupported
	}
	if r.StatusCode() >= 400 {
		return nil, fmt.Errorf("get user delta http status %d: %s", r.StatusCode(), bodySnippet(r.Body()))
	}

	var response UserDeltaResponse
	if err := json.Unmarshal(r.Body(), &response); err != nil {
		return nil, fmt.Errorf("decode user delta error: %w; body=%s", err, bodySnippet(r.Body()))
	}
	return &response.Data, nil
}

func (c *Client) SetUserSyncSeq(seq int64) {
	if seq >= 0 {
		c.userSyncSeq = seq
	}
}

func (c *Client) UserSyncSeq() int64 {
	return c.userSyncSeq
}

func (c *Client) updateUserSyncSeqFromHeader(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return
	}
	c.userSyncSeq = seq
}

// GetUserAlive will fetch the alive_ip count for users
func (c *Client) GetUserAlive(ctx context.Context) (map[int]int, error) {
	c.AliveMap = &AliveMap{}
	const path = "/api/v1/server/UniProxy/alivelist"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	if r == nil || r.RawResponse == nil || r.StatusCode() >= 399 {
		c.AliveMap.Alive = make(map[int]int)
		return c.AliveMap.Alive, nil
	}
	defer r.RawResponse.Body.Close()
	if err := json.Unmarshal(r.Body(), c.AliveMap); err != nil {
		fmt.Printf("unmarshal user alive list error: %s", err)
		c.AliveMap.Alive = make(map[int]int)
	}

	return c.AliveMap.Alive, nil
}

func (c *Client) GetUserDeviceAlive(ctx context.Context) (map[int]int, error) {
	deviceAlive := &DeviceAliveMap{}
	const path = "/api/v1/server/UniProxy/deviceAliveList"
	r, err := c.client.R().
		SetContext(ctx).
		ForceContentType("application/json").
		Get(path)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		deviceAlive.AliveDevices = make(map[int]int)
		return deviceAlive.AliveDevices, nil
	}
	if r == nil || r.RawResponse == nil || r.StatusCode() >= 399 {
		deviceAlive.AliveDevices = make(map[int]int)
		return deviceAlive.AliveDevices, nil
	}
	defer r.RawResponse.Body.Close()
	if err := json.Unmarshal(r.Body(), deviceAlive); err != nil {
		fmt.Printf("unmarshal user device alive list error: %s", err)
		deviceAlive.AliveDevices = make(map[int]int)
	}
	if deviceAlive.AliveDevices == nil {
		deviceAlive.AliveDevices = make(map[int]int)
	}

	return deviceAlive.AliveDevices, nil
}

type UserTraffic struct {
	UID      int
	Upload   int64
	Download int64
}

type UserDeviceTraffic struct {
	UID      int
	UUID     string
	Upload   int64
	Download int64
}

type DeviceTrafficReportItem struct {
	UUID     string `json:"uuid"`
	Upload   int64  `json:"u"`
	Download int64  `json:"d"`
}

// ReportUserTraffic reports the user traffic
func (c *Client) ReportUserTraffic(ctx context.Context, userTraffic []UserTraffic) error {
	data := make(map[int][]int64, len(userTraffic))
	for i := range userTraffic {
		if current, ok := data[userTraffic[i].UID]; ok {
			current[0] += userTraffic[i].Upload
			current[1] += userTraffic[i].Download
		} else {
			data[userTraffic[i].UID] = []int64{userTraffic[i].Upload, userTraffic[i].Download}
		}
	}
	const path = "/api/v1/server/UniProxy/push"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) ReportUserDeviceTraffic(ctx context.Context, userDeviceTraffic []UserDeviceTraffic) error {
	data := make(map[int][]DeviceTrafficReportItem, len(userDeviceTraffic))
	for i := range userDeviceTraffic {
		if userDeviceTraffic[i].UID <= 0 || userDeviceTraffic[i].UUID == "" {
			continue
		}
		data[userDeviceTraffic[i].UID] = append(data[userDeviceTraffic[i].UID], DeviceTrafficReportItem{
			UUID:     userDeviceTraffic[i].UUID,
			Upload:   userDeviceTraffic[i].Upload,
			Download: userDeviceTraffic[i].Download,
		})
	}
	if len(data) == 0 {
		return nil
	}
	const path = "/api/v1/server/UniProxy/pushDevices"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) ReportNodeOnlineUsers(ctx context.Context, data *map[int][]string) error {
	const path = "/api/v1/server/UniProxy/alive"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)

	if err != nil {
		return err
	}

	return nil
}

func (c *Client) ReportNodeOnlineDevices(ctx context.Context, data *map[int][]OnlineDeviceReportItem) error {
	const path = "/api/v1/server/UniProxy/aliveDevices"
	_, err := c.client.R().
		SetContext(ctx).
		SetBody(data).
		ForceContentType("application/json").
		Post(path)

	if err != nil {
		return err
	}

	return nil
}
