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
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type Config struct {
	Enabled        bool
	Endpoint       string
	Token          string
	BatchSize      int
	MaxQueueSize   int
	FlushInterval  time.Duration
	Timeout        time.Duration
	PersistTimeout time.Duration
	SpoolPath      string
	MaxSpoolBytes  int64
	MaxSpoolAge    time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
	MkdirAll       func(string, os.FileMode) error
	FlowTraffic    FlowConfig
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

type AccessRuntimeStatus struct {
	ConfigReported            bool
	Enabled                   bool
	LastSuccessAt             int64
	LastErrorAt               int64
	LastErrorCode             string
	RetryCount                uint64
	PendingEvents             uint64
	PendingBytes              uint64
	OldestEventAt             int64
	DroppedEvents             uint64
	DroppedBytes              uint64
	DroppedEventFrom          int64
	DroppedEventTo            int64
	RejectedEvents            uint64
	RejectedBytes             uint64
	RejectedEventFrom         int64
	RejectedEventTo           int64
	PersistenceFailures       uint64
	PersistenceFailureBytes   uint64
	PersistenceFailureFrom    int64
	PersistenceFailureTo      int64
	PersistQueueDepth         uint64
	PersistQueueHighWatermark uint64
	PersistTimeouts           uint64
	LastMigrationAt           int64
	LastMigrationRecords      uint64
	LastMigrationMillis       int64
}

type accessSendError struct {
	code   string
	status int
	err    error
}

func (e *accessSendError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("access audit status %d", e.status)
}

func (e *accessSendError) Unwrap() error { return e.err }

type Client struct {
	config    Config
	spool     AccessSpool
	persister *persistBatcher[Event]
	closeCh   chan struct{}
	doneCh    chan struct{}
	wakeCh    chan struct{}
	started   bool
	startMu   sync.Mutex
	closeOnce sync.Once

	statusMu sync.RWMutex
	status   AccessRuntimeStatus
	retryAt  time.Time

	persistLogMu   sync.Mutex
	persistLastLog time.Time
	persistFailing bool
}

var (
	defaultMu           sync.RWMutex
	defaultClient       *Client
	defaultFlowClient   *FlowClient
	defaultFlowConfig   FlowConfig
	defaultAccessStatus AccessRuntimeStatus
	defaultFlowStatus   FlowRuntimeStatus
	flowConfigReported  bool
)

func Configure(config Config) error {
	config = normalizeConfig(config)
	oldClient, oldFlowClient := detachDefaultClients()
	closeClients(oldClient, oldFlowClient)

	defaultMu.Lock()
	defaultFlowConfig = config.FlowTraffic
	flowConfigReported = true
	defaultAccessStatus = AccessRuntimeStatus{ConfigReported: true, Enabled: config.Enabled}
	defaultFlowStatus = FlowRuntimeStatus{ConfigReported: true, Enabled: config.FlowTraffic.Enabled}
	defaultMu.Unlock()
	if !config.Enabled {
		return nil
	}

	client, err := NewClient(config)
	if err != nil && !errors.Is(err, ErrSpoolOpen) {
		return err
	}
	if errors.Is(err, ErrSpoolOpen) {
		recordAccessStartupFailure(config.Now(), err)
		client = nil
	}

	var flowClient *FlowClient
	if config.FlowTraffic.Enabled {
		spool, spoolErr := NewBoltFlowSpool(SpoolConfig{
			Path:     config.FlowTraffic.SpoolPath,
			MaxBytes: config.FlowTraffic.MaxSpoolBytes,
			MaxAge:   config.FlowTraffic.MaxSpoolAge,
			Now:      config.Now,
			MkdirAll: config.MkdirAll,
		})
		if spoolErr != nil {
			recordFlowStartupFailure(config.Now(), spoolErr)
		} else {
			flowClient, spoolErr = NewFlowClient(FlowClientConfig{
				Enabled:        true,
				Endpoint:       config.Endpoint,
				Token:          config.Token,
				BatchSize:      config.BatchSize,
				MaxQueueSize:   config.MaxQueueSize,
				FlushInterval:  config.FlushInterval,
				Timeout:        config.Timeout,
				PersistTimeout: config.PersistTimeout,
				HTTPClient:     config.HTTPClient,
				Now:            config.Now,
				Spool:          spool,
			})
			if spoolErr != nil {
				_ = spool.Close()
				if client != nil {
					client.Close()
				}
				return spoolErr
			}
		}
	}

	if client != nil {
		client.Start()
	}
	if flowClient != nil {
		flowClient.Start()
	}
	defaultMu.Lock()
	defaultClient = client
	defaultFlowClient = flowClient
	defaultMu.Unlock()
	return nil
}

func Shutdown() {
	oldClient, oldFlowClient := detachDefaultClients()
	closeClients(oldClient, oldFlowClient)
	defaultMu.Lock()
	defaultFlowConfig = FlowConfig{}
	defaultAccessStatus = AccessRuntimeStatus{}
	defaultFlowStatus = FlowRuntimeStatus{}
	defaultMu.Unlock()
}

func detachDefaultClients() (*Client, *FlowClient) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	client, flowClient := defaultClient, defaultFlowClient
	defaultClient, defaultFlowClient = nil, nil
	return client, flowClient
}

func closeClients(client *Client, flowClient *FlowClient) {
	if client != nil {
		client.Close()
	}
	if flowClient != nil {
		flowClient.Close()
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

func ReportFlow(event FlowEvent) error {
	defaultMu.RLock()
	client := defaultFlowClient
	defaultMu.RUnlock()
	if client == nil {
		return errors.New("flow traffic reporting is disabled")
	}
	return client.Report(event)
}

func FlowTrafficEnabled() bool {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultFlowClient != nil && defaultFlowConfig.Enabled
}

func FlowCheckpointInterval() time.Duration {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultFlowConfig.CheckpointInterval
}

func CurrentRuntimeStatus() AccessRuntimeStatus {
	defaultMu.RLock()
	client := defaultClient
	fallback := defaultAccessStatus
	defaultMu.RUnlock()
	if client != nil {
		return client.Status()
	}
	return fallback
}

func CurrentFlowRuntimeStatus() FlowRuntimeStatus {
	defaultMu.RLock()
	client := defaultFlowClient
	fallback := defaultFlowStatus
	config := defaultFlowConfig
	reported := flowConfigReported
	defaultMu.RUnlock()
	if client != nil {
		return client.Status()
	}
	if fallback.ConfigReported {
		return fallback
	}
	return FlowRuntimeStatus{ConfigReported: reported, Enabled: config.Enabled}
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
	client := &Client{
		config:  config,
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
		wakeCh:  make(chan struct{}, 1),
		status: AccessRuntimeStatus{
			ConfigReported: true,
			Enabled:        config.Enabled,
		},
	}
	if !config.Enabled {
		return client, nil
	}
	spool, err := NewBoltAccessSpool(SpoolConfig{
		Path:     config.SpoolPath,
		MaxBytes: config.MaxSpoolBytes,
		MaxAge:   config.MaxSpoolAge,
		Now:      config.Now,
		MkdirAll: config.MkdirAll,
	})
	if err != nil {
		return nil, err
	}
	client.spool = spool
	client.persister = newPersistBatcher(persistBatcherConfig[Event]{
		Spool:         spool,
		QueueSize:     config.MaxQueueSize,
		BatchSize:     config.BatchSize,
		BatchWindow:   10 * time.Millisecond,
		SubmitTimeout: config.PersistTimeout,
	})
	return client, nil
}

func (c *Client) Start() {
	if c == nil || !c.config.Enabled {
		return
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.started {
		return
	}
	c.started = true
	c.persister.Start()
	go c.loop()
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

func (c *Client) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		persistenceClosed := true
		if c.persister != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := c.persister.Close(ctx)
			cancel()
			if err != nil {
				persistenceClosed = false
				log.WithField("err", err).Warn("SNTP access audit persistence shutdown timed out")
			}
		}
		c.startMu.Lock()
		started := c.started
		if started {
			close(c.closeCh)
		}
		c.startMu.Unlock()
		if started {
			<-c.doneCh
		}
		if c.spool != nil && persistenceClosed {
			_ = c.spool.Close()
		}
	})
}

func (c *Client) Enqueue(event Event) bool {
	if c == nil || !c.config.Enabled || c.persister == nil {
		return false
	}
	if err := normalizeAccessEvent(&event, c.config.Now()); err != nil {
		log.WithField("err", err).Warn("SNTP access audit event rejected before persistence")
		return false
	}
	if err := c.persister.Submit(event); err != nil {
		encoded, _ := json.Marshal(event)
		c.spool.RecordPersistenceFailure(event.EventTime, uint64(len(encoded)))
		c.recordPersistenceLog(err)
		return false
	}
	c.recordPersistenceRecovery()
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
	return true
}

func (c *Client) Dropped() uint64 {
	if c == nil {
		return 0
	}
	status := c.Status()
	return status.DroppedEvents + status.PersistenceFailures
}

func (c *Client) Status() AccessRuntimeStatus {
	if c == nil {
		return AccessRuntimeStatus{}
	}
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if c.spool != nil {
		stats, err := c.spool.Stats()
		if err == nil {
			status.PendingEvents = stats.PendingEvents
			status.PendingBytes = stats.PendingBytes
			status.OldestEventAt = stats.OldestEventAt
			status.DroppedEvents = stats.DroppedEvents
			status.DroppedBytes = stats.DroppedBytes
			status.DroppedEventFrom = stats.DroppedEventFrom
			status.DroppedEventTo = stats.DroppedEventTo
			status.RejectedEvents = stats.RejectedEvents
			status.RejectedBytes = stats.RejectedBytes
			status.RejectedEventFrom = stats.RejectedEventFrom
			status.RejectedEventTo = stats.RejectedEventTo
			status.PersistenceFailures = stats.PersistenceFailures
			status.PersistenceFailureBytes = stats.PersistenceFailureBytes
			status.PersistenceFailureFrom = stats.PersistenceFailureFrom
			status.PersistenceFailureTo = stats.PersistenceFailureTo
			status.LastMigrationAt = stats.LastMigrationAt
			status.LastMigrationRecords = stats.LastMigrationRecords
			status.LastMigrationMillis = stats.LastMigrationMillis
		}
	}
	if c.persister != nil {
		persist := c.persister.Status()
		status.PersistQueueDepth = persist.QueueDepth
		status.PersistQueueHighWatermark = persist.HighWatermark
		status.PersistTimeouts = persist.Timeouts
	}
	return status
}

func (c *Client) loop() {
	defer close(c.doneCh)
	ticker := time.NewTicker(c.config.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.wakeCh:
			c.flushIfReady()
		case <-ticker.C:
			c.flushIfReady()
		case <-c.closeCh:
			return
		}
	}
}

func (c *Client) flushIfReady() {
	c.statusMu.RLock()
	retryAt := c.retryAt
	c.statusMu.RUnlock()
	if !retryAt.IsZero() && c.config.Now().Before(retryAt) {
		return
	}
	_ = c.flushOnce()
}

func (c *Client) flushOnce() error {
	items, err := c.spool.Peek(c.config.BatchSize)
	if err != nil {
		c.recordFailure("spool", err)
		return err
	}
	if len(items) == 0 {
		return nil
	}
	rejected, sendErr := c.sendItems(items)
	if sendErr != nil {
		c.recordFailure(sendErr.code, sendErr)
		return sendErr
	}
	c.recordSuccess(rejected)
	return nil
}

func (c *Client) sendItems(items []AccessSpoolItem) (bool, *accessSendError) {
	statusCode, err := c.post(items)
	if err != nil {
		return false, classifyAccessTransportError(err)
	}
	if statusCode >= 200 && statusCode < 300 {
		keys := make([]uint64, 0, len(items))
		for _, item := range items {
			keys = append(keys, item.Key)
		}
		if err := c.spool.Ack(keys); err != nil {
			return false, &accessSendError{code: "spool", err: err}
		}
		return false, nil
	}
	if statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge {
		if len(items) == 1 {
			if err := c.spool.Reject([]uint64{items[0].Key}); err != nil {
				return false, &accessSendError{code: "spool", err: err}
			}
			return true, nil
		}
		middle := len(items) / 2
		leftRejected, leftErr := c.sendItems(items[:middle])
		if leftErr != nil {
			return leftRejected, leftErr
		}
		rightRejected, rightErr := c.sendItems(items[middle:])
		return leftRejected || rightRejected, rightErr
	}
	return false, classifyAccessStatus(statusCode)
}

func (c *Client) post(items []AccessSpoolItem) (int, error) {
	events := make([]Event, 0, len(items))
	for _, item := range items {
		events = append(events, item.Event)
	}
	body, err := encodePayload(events, c.config.Now())
	if err != nil {
		return 0, err
	}
	timestamp := fmt.Sprintf("%d", c.config.Now().Unix())
	ctx, cancel := context.WithTimeout(context.Background(), c.config.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-SNTP-Timestamp", timestamp)
	req.Header.Set("X-SNTP-Signature", sign(body, c.config.Token, timestamp))
	resp, err := c.config.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func (c *Client) recordSuccess(rejected bool) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.LastSuccessAt = c.config.Now().Unix()
	c.status.RetryCount = 0
	c.retryAt = time.Time{}
	if rejected {
		c.status.LastErrorAt = c.config.Now().Unix()
		c.status.LastErrorCode = "invalid_event"
		return
	}
	c.status.LastErrorCode = ""
}

func (c *Client) recordFailure(code string, err error) {
	_ = err
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.status.LastErrorAt = c.config.Now().Unix()
	c.status.LastErrorCode = code
	c.status.RetryCount++
	shift := c.status.RetryCount - 1
	if shift > 6 {
		shift = 6
	}
	c.retryAt = c.config.Now().Add(time.Second * time.Duration(1<<shift))
}

func (c *Client) recordPersistenceLog(err error) {
	now := c.config.Now()
	c.persistLogMu.Lock()
	defer c.persistLogMu.Unlock()
	if !c.persistFailing || now.Sub(c.persistLastLog) >= time.Minute {
		log.WithField("err", err).Warn("SNTP access audit local persistence failed")
		c.persistLastLog = now
	}
	c.persistFailing = true
}

func (c *Client) recordPersistenceRecovery() {
	c.persistLogMu.Lock()
	defer c.persistLogMu.Unlock()
	if !c.persistFailing {
		return
	}
	log.Info("SNTP access audit local persistence recovered")
	c.persistFailing = false
}

func recordAccessStartupFailure(now time.Time, err error) {
	log.WithField("err", err).Error("SNTP access audit spool unavailable; proxy traffic remains enabled")
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultAccessStatus.LastErrorAt = now.Unix()
	defaultAccessStatus.LastErrorCode = "spool_open"
	defaultAccessStatus.PersistenceFailures++
	defaultAccessStatus.PersistenceFailureFrom = now.Unix()
	defaultAccessStatus.PersistenceFailureTo = now.Unix()
}

func recordFlowStartupFailure(now time.Time, err error) {
	log.WithField("err", err).Error("SNTP flow audit spool unavailable; proxy traffic remains enabled")
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultFlowStatus.LastErrorAt = now.Unix()
	defaultFlowStatus.LastErrorCode = "spool_open"
	defaultFlowStatus.PersistenceFailures++
	defaultFlowStatus.PersistenceFailureFrom = now.Unix()
	defaultFlowStatus.PersistenceFailureTo = now.Unix()
}

func classifyAccessTransportError(err error) *accessSendError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &accessSendError{code: "timeout", err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &accessSendError{code: "timeout", err: err}
	}
	return &accessSendError{code: "network", err: err}
}

func classifyAccessStatus(statusCode int) *accessSendError {
	code := "remote_status"
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		code = "auth"
	case statusCode == http.StatusRequestTimeout:
		code = "timeout"
	case statusCode == http.StatusTooManyRequests:
		code = "rate_limited"
	case statusCode >= 500:
		code = "clickhouse_5xx"
	}
	return &accessSendError{code: code, status: statusCode}
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

func normalizeAccessEvent(event *Event, now time.Time) error {
	if event.EventTime.IsZero() {
		event.EventTime = now
	}
	event.NodeTag = strings.TrimSpace(event.NodeTag)
	event.UUID = strings.TrimSpace(event.UUID)
	event.SourceIP = strings.TrimSpace(event.SourceIP)
	event.TargetHost = strings.TrimSpace(event.TargetHost)
	event.Network = strings.ToLower(strings.TrimSpace(event.Network))
	event.InboundTag = strings.TrimSpace(event.InboundTag)
	event.OutboundTag = strings.TrimSpace(event.OutboundTag)
	_, err := event.toWire(now)
	return err
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
	config.SpoolPath = strings.TrimSpace(config.SpoolPath)
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
	if config.PersistTimeout <= 0 {
		config.PersistTimeout = time.Second
	}
	if config.SpoolPath == "" {
		config.SpoolPath = defaultAccessSpoolPath
	}
	if config.MaxSpoolBytes <= 0 {
		config.MaxSpoolBytes = defaultAccessSpoolMaxBytes
	}
	if config.MaxSpoolAge <= 0 {
		config.MaxSpoolAge = defaultAccessSpoolMaxAge
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.Timeout}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MkdirAll == nil {
		config.MkdirAll = os.MkdirAll
	}
	return config
}

func sign(body []byte, token string, timestamp string) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	mac.Write([]byte(timestamp))
	return hex.EncodeToString(mac.Sum(nil))
}
