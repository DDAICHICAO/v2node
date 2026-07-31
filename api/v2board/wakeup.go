package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"encoding/json/v2"

	"github.com/gorilla/websocket"
	selfversion "github.com/wyx2685/v2node/common/version"
)

const maxWakeupMessageBytes = 4096

type UserSyncWakeupMessage struct {
	Type        string `json:"type"`
	Revision    int64  `json:"revision"`
	PublishedAt int64  `json:"published_at"`
	SentAt      int64  `json:"sent_at"`
}

type UserSyncAppliedMessage struct {
	Type      string `json:"type"`
	Revision  int64  `json:"revision"`
	SyncSeq   int64  `json:"sync_seq"`
	AppliedAt int64  `json:"applied_at"`
}

type UserSyncPongMessage struct {
	Type       string `json:"type"`
	ReceivedAt int64  `json:"received_at"`
}

type UserSyncWakeupConnection struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *Client) UserSyncWakeupDialConfig(
	cfg *UserSyncWakeupConfig,
) (string, http.Header, error) {
	if c == nil || cfg == nil {
		return "", nil, errors.New("user sync wakeup config is required")
	}
	if c.Token == "" || c.NodeId <= 0 || c.InstanceID() == "" {
		return "", nil, errors.New("user sync wakeup identity is incomplete")
	}

	base, err := url.Parse(c.APIHost)
	if err != nil || base.Host == "" {
		return "", nil, errors.New("invalid panel api host")
	}
	switch strings.ToLower(base.Scheme) {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	default:
		return "", nil, errors.New("unsupported panel api scheme")
	}

	normalized := *cfg
	normalized.Normalize()
	path, err := url.Parse(normalized.Path)
	if err != nil || path.IsAbs() || path.Host != "" ||
		path.RawQuery != "" || path.Fragment != "" ||
		!strings.HasPrefix(path.Path, "/") {
		return "", nil, errors.New("invalid user sync wakeup path")
	}
	base.Path = strings.TrimRight(base.Path, "/") + path.Path
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+c.Token)
	headers.Set("X-SNTP-Node-Type", "v2node")
	headers.Set("X-SNTP-Node-ID", strconv.Itoa(c.NodeId))
	headers.Set("X-SNTP-Instance-ID", c.InstanceID())
	headers.Set("X-SNTP-Version", selfversion.Current())
	headers.Set("X-SNTP-Capabilities", strings.Join(deviceLimitCapabilities, ","))

	return base.String(), headers, nil
}

func (c *Client) DialUserSyncWakeup(
	ctx context.Context,
	cfg *UserSyncWakeupConfig,
) (*UserSyncWakeupConnection, error) {
	endpoint, headers, err := c.UserSyncWakeupDialConfig(cfg)
	if err != nil {
		return nil, err
	}
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf(
				"user sync wakeup dial failed: status=%d",
				response.StatusCode,
			)
		}
		return nil, fmt.Errorf("user sync wakeup dial failed: %T", err)
	}
	conn.SetReadLimit(maxWakeupMessageBytes)
	return &UserSyncWakeupConnection{conn: conn}, nil
}

func (c *UserSyncWakeupConnection) ReadMessage(
	ctx context.Context,
) (*UserSyncWakeupMessage, error) {
	if c == nil || c.conn == nil {
		return nil, errors.New("user sync wakeup connection is closed")
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetReadDeadline(deadline)
	} else {
		_ = c.conn.SetReadDeadline(time.Time{})
	}
	_, payload, err := c.conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("read user sync wakeup message: %T", err)
	}
	return decodeWakeupMessage(payload)
}

func decodeWakeupMessage(payload []byte) (*UserSyncWakeupMessage, error) {
	var message UserSyncWakeupMessage
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, errors.New("invalid user sync wakeup message")
	}
	switch message.Type {
	case "user_sync_dirty":
		if message.Revision <= 0 || message.PublishedAt < 0 {
			return nil, errors.New("invalid user sync dirty message")
		}
	case "ping":
		if message.SentAt <= 0 {
			return nil, errors.New("invalid user sync ping message")
		}
	default:
		return nil, errors.New("unsupported user sync wakeup message")
	}
	return &message, nil
}

func (c *UserSyncWakeupConnection) SendApplied(
	revision int64,
	syncSeq int64,
	appliedAt int64,
) error {
	if revision <= 0 || syncSeq < 0 || appliedAt <= 0 {
		return errors.New("invalid user sync applied message")
	}
	return c.writeJSON(UserSyncAppliedMessage{
		Type:      "user_sync_applied",
		Revision:  revision,
		SyncSeq:   syncSeq,
		AppliedAt: appliedAt,
	})
}

func (c *UserSyncWakeupConnection) SendPong(receivedAt int64) error {
	if receivedAt <= 0 {
		return errors.New("invalid user sync pong message")
	}
	return c.writeJSON(UserSyncPongMessage{
		Type:       "pong",
		ReceivedAt: receivedAt,
	})
}

func (c *UserSyncWakeupConnection) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *UserSyncWakeupConnection) writeJSON(value any) error {
	if c == nil || c.conn == nil {
		return errors.New("user sync wakeup connection is closed")
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return errors.New("encode user sync wakeup message")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("write user sync wakeup message: %T", err)
	}
	return nil
}
