package accessaudit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	Enabled       bool
	Endpoint      string
	Token         string
	BatchSize     int
	MaxQueueSize  int
	FlushInterval time.Duration
	Timeout       time.Duration
	HTTPClient    *http.Client
	Now           func() time.Time
}

type Event struct {
	EventTime   time.Time `json:"event_time"`
	NodeID      uint32    `json:"node_id"`
	NodeTag     string    `json:"node_tag"`
	UID         uint64    `json:"uid"`
	UUID        string    `json:"uuid"`
	SourceIP    string    `json:"source_ip"`
	TargetHost  string    `json:"target_host"`
	TargetPort  uint16    `json:"target_port"`
	Network     string    `json:"network"`
	InboundTag  string    `json:"inbound_tag"`
	OutboundTag string    `json:"outbound_tag"`
}

type wireEvent struct {
	EventTime   string `json:"event_time"`
	NodeID      uint32 `json:"node_id"`
	NodeTag     string `json:"node_tag"`
	UID         uint64 `json:"uid"`
	UUID        string `json:"uuid"`
	SourceIP    string `json:"source_ip"`
	TargetHost  string `json:"target_host"`
	TargetPort  uint16 `json:"target_port"`
	Network     string `json:"network"`
	InboundTag  string `json:"inbound_tag"`
	OutboundTag string `json:"outbound_tag"`
}

type payload struct {
	Events []wireEvent `json:"events"`
}

type Client struct {
	config    Config
	queue     chan Event
	closeCh   chan struct{}
	doneCh    chan struct{}
	dropped   atomic.Uint64
	started   atomic.Bool
	closeOnce sync.Once
}

var (
	defaultMu     sync.RWMutex
	defaultClient *Client
)

func Configure(config Config) error {
	defaultMu.Lock()
	defer defaultMu.Unlock()

	if defaultClient != nil {
		defaultClient.Close()
		defaultClient = nil
	}
	if !config.Enabled {
		return nil
	}
	client, err := NewClient(config)
	if err != nil {
		return err
	}
	client.Start()
	defaultClient = client
	return nil
}

func Shutdown() {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if defaultClient != nil {
		defaultClient.Close()
		defaultClient = nil
	}
}

func Enqueue(event Event) bool {
	defaultMu.RLock()
	client := defaultClient
	defaultMu.RUnlock()
	if client == nil {
		return false
	}
	return client.Enqueue(event)
}

func NewClient(config Config) (*Client, error) {
	config = normalizeConfig(config)
	if config.Enabled {
		if config.Endpoint == "" {
			return nil, errors.New("access audit endpoint is required")
		}
		if config.Token == "" {
			return nil, errors.New("access audit token is required")
		}
	}
	return &Client{
		config:  config,
		queue:   make(chan Event, config.MaxQueueSize),
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
	}, nil
}

func (c *Client) Start() {
	if c == nil || !c.config.Enabled || !c.started.CompareAndSwap(false, true) {
		return
	}
	go c.loop()
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.closeCh)
		if c.started.Load() {
			<-c.doneCh
		}
	})
}

func (c *Client) Enqueue(event Event) bool {
	if c == nil || !c.config.Enabled {
		return false
	}
	select {
	case c.queue <- event:
		return true
	default:
		c.dropped.Add(1)
		return false
	}
}

func (c *Client) Dropped() uint64 {
	if c == nil {
		return 0
	}
	return c.dropped.Load()
}

func (c *Client) loop() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.config.FlushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, c.config.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := c.send(batch); err != nil {
			log.WithField("err", err).Warn("SNTP access audit report failed")
		}
		batch = batch[:0]
	}

	for {
		select {
		case event := <-c.queue:
			batch = append(batch, event)
			if len(batch) >= c.config.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.closeCh:
			for {
				select {
				case event := <-c.queue:
					batch = append(batch, event)
					if len(batch) >= c.config.BatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (c *Client) send(events []Event) error {
	body, err := encodePayload(events, c.config.Now())
	if err != nil {
		return err
	}
	timestamp := fmt.Sprintf("%d", c.config.Now().Unix())
	signature := sign(body, c.config.Token, timestamp)

	ctx, cancel := context.WithTimeout(context.Background(), c.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SNTP-Timestamp", timestamp)
	req.Header.Set("X-SNTP-Signature", signature)

	resp, err := c.config.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusMultipleChoices {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("access audit status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func encodePayload(events []Event, now time.Time) ([]byte, error) {
	out := payload{Events: make([]wireEvent, 0, len(events))}
	for _, event := range events {
		wire, err := event.toWire(now)
		if err != nil {
			return nil, err
		}
		out.Events = append(out.Events, wire)
	}
	return json.Marshal(out)
}

func (e Event) toWire(now time.Time) (wireEvent, error) {
	if e.EventTime.IsZero() {
		e.EventTime = now
	}
	e.Network = strings.ToLower(strings.TrimSpace(e.Network))
	if e.NodeID == 0 {
		return wireEvent{}, errors.New("access audit node_id is required")
	}
	if e.UID == 0 {
		return wireEvent{}, errors.New("access audit uid is required")
	}
	if strings.TrimSpace(e.UUID) == "" {
		return wireEvent{}, errors.New("access audit uuid is required")
	}
	if strings.TrimSpace(e.SourceIP) == "" {
		return wireEvent{}, errors.New("access audit source_ip is required")
	}
	if strings.TrimSpace(e.TargetHost) == "" {
		return wireEvent{}, errors.New("access audit target_host is required")
	}
	if e.TargetPort == 0 {
		return wireEvent{}, errors.New("access audit target_port is required")
	}
	if e.Network != "tcp" && e.Network != "udp" {
		return wireEvent{}, errors.New("access audit network must be tcp or udp")
	}
	return wireEvent{
		EventTime:   e.EventTime.Format(time.RFC3339Nano),
		NodeID:      e.NodeID,
		NodeTag:     strings.TrimSpace(e.NodeTag),
		UID:         e.UID,
		UUID:        strings.TrimSpace(e.UUID),
		SourceIP:    strings.TrimSpace(e.SourceIP),
		TargetHost:  strings.TrimSpace(e.TargetHost),
		TargetPort:  e.TargetPort,
		Network:     e.Network,
		InboundTag:  strings.TrimSpace(e.InboundTag),
		OutboundTag: strings.TrimSpace(e.OutboundTag),
	}, nil
}

func normalizeConfig(config Config) Config {
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	config.Token = strings.TrimSpace(config.Token)
	if config.BatchSize <= 0 {
		config.BatchSize = 1000
	}
	if config.MaxQueueSize <= 0 {
		config.MaxQueueSize = 10000
	}
	if config.FlushInterval <= 0 {
		config.FlushInterval = time.Second
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.Timeout}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return config
}

func sign(body []byte, token string, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	mac.Write([]byte(timestamp))
	return hex.EncodeToString(mac.Sum(nil))
}
