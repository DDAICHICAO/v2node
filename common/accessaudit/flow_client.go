package accessaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

type FlowConfig struct {
	Enabled            bool
	CheckpointInterval time.Duration
	SpoolPath          string
	MaxSpoolBytes      int64
	MaxSpoolAge        time.Duration
}

type FlowClientConfig struct {
	Enabled        bool
	Endpoint       string
	Token          string
	BatchSize      int
	MaxQueueSize   int
	FlushInterval  time.Duration
	Timeout        time.Duration
	PersistTimeout time.Duration
	HTTPClient     *http.Client
	Now            func() time.Time
	Spool          FlowSpool
}

type FlowRuntimeStatus struct {
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

type flowPayload struct {
	EventType string      `json:"event_type"`
	Events    []FlowEvent `json:"events"`
}

type flowSendError struct {
	code   string
	status int
	err    error
}

func (e *flowSendError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("flow traffic status %d", e.status)
}

func (e *flowSendError) Unwrap() error { return e.err }

type FlowClient struct {
	config    FlowClientConfig
	spool     FlowSpool
	persister *persistBatcher[FlowEvent]
	closeCh   chan struct{}
	doneCh    chan struct{}
	wakeCh    chan struct{}
	started   bool
	startMu   sync.Mutex
	closeOnce sync.Once

	statusMu sync.RWMutex
	status   FlowRuntimeStatus
	retryAt  time.Time

	persistLogMu   sync.Mutex
	persistLastLog time.Time
	persistFailing bool
}

func NewFlowClient(config FlowClientConfig) (*FlowClient, error) {
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
	if config.PersistTimeout <= 0 {
		config.PersistTimeout = time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: config.Timeout}
	}
	if config.Enabled {
		if config.Endpoint == "" {
			return nil, errors.New("flow traffic endpoint is required")
		}
		if config.Token == "" {
			return nil, errors.New("flow traffic token is required")
		}
		if config.Spool == nil {
			return nil, errors.New("flow traffic spool is required")
		}
	}
	client := &FlowClient{
		config:  config,
		spool:   config.Spool,
		closeCh: make(chan struct{}),
		doneCh:  make(chan struct{}),
		wakeCh:  make(chan struct{}, 1),
		status: FlowRuntimeStatus{
			ConfigReported: true,
			Enabled:        config.Enabled,
		},
	}
	if config.Enabled {
		client.persister = newPersistBatcher(persistBatcherConfig[FlowEvent]{
			Spool:         config.Spool,
			QueueSize:     config.MaxQueueSize,
			BatchSize:     config.BatchSize,
			BatchWindow:   10 * time.Millisecond,
			SubmitTimeout: config.PersistTimeout,
			OnSuccess: func([]FlowEvent) {
				client.recordPersistenceRecovery()
				select {
				case client.wakeCh <- struct{}{}:
				default:
				}
			},
			OnFailure: client.recordPersistenceFailures,
		})
	}
	return client, nil
}

func (c *FlowClient) Start() {
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
}

func (c *FlowClient) Close() {
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
				log.WithField("err", err).Warn("SNTP flow audit persistence shutdown timed out")
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

func (c *FlowClient) Report(event FlowEvent) error {
	if c == nil || !c.config.Enabled || c.spool == nil || c.persister == nil {
		return errors.New("flow traffic reporting is disabled")
	}
	if err := event.Normalize(c.config.Now()); err != nil {
		return err
	}
	if err := c.persister.TrySubmit(event); err != nil {
		if errors.Is(err, ErrPersistenceQueueFull) || errors.Is(err, ErrPersistenceClosed) {
			c.recordPersistenceFailures([]FlowEvent{event}, err)
		}
		return err
	}
	return nil
}

func (c *FlowClient) Status() FlowRuntimeStatus {
	if c == nil {
		return FlowRuntimeStatus{}
	}
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if c.spool == nil {
		return status
	}
	stats, err := c.spool.Stats()
	if err != nil {
		return status
	}
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
	if c.persister != nil {
		persist := c.persister.Status()
		status.PersistQueueDepth = persist.QueueDepth
		status.PersistQueueHighWatermark = persist.HighWatermark
		status.PersistTimeouts = persist.Timeouts
	}
	return status
}

func (c *FlowClient) recordPersistenceLog(err error) {
	now := c.config.Now()
	c.persistLogMu.Lock()
	defer c.persistLogMu.Unlock()
	if !c.persistFailing || now.Sub(c.persistLastLog) >= time.Minute {
		log.WithField("err", err).Warn("SNTP flow audit local persistence failed")
		c.persistLastLog = now
	}
	c.persistFailing = true
}

func (c *FlowClient) recordPersistenceFailures(events []FlowEvent, err error) {
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		c.spool.RecordPersistenceFailure(event.EventTime, uint64(len(encoded)))
	}
	c.recordPersistenceLog(err)
}

func (c *FlowClient) recordPersistenceRecovery() {
	c.persistLogMu.Lock()
	defer c.persistLogMu.Unlock()
	if !c.persistFailing {
		return
	}
	log.Info("SNTP flow audit local persistence recovered")
	c.persistFailing = false
}

func (c *FlowClient) loop() {
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

func (c *FlowClient) flushIfReady() {
	c.statusMu.RLock()
	retryAt := c.retryAt
	c.statusMu.RUnlock()
	if !retryAt.IsZero() && c.config.Now().Before(retryAt) {
		return
	}
	_ = c.flushOnce()
}

func (c *FlowClient) flushOnce() error {
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

func (c *FlowClient) sendItems(items []SpoolItem) (bool, *flowSendError) {
	statusCode, err := c.post(items)
	if err != nil {
		return false, classifyFlowTransportError(err)
	}
	if statusCode >= 200 && statusCode < 300 {
		keys := make([]uint64, 0, len(items))
		for _, item := range items {
			keys = append(keys, item.Key)
		}
		if err := c.spool.Ack(keys); err != nil {
			return false, &flowSendError{code: "spool", err: err}
		}
		return false, nil
	}
	if statusCode == http.StatusBadRequest || statusCode == http.StatusRequestEntityTooLarge {
		if len(items) == 1 {
			if err := c.spool.Reject([]uint64{items[0].Key}); err != nil {
				return false, &flowSendError{code: "spool", err: err}
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
	return false, classifyFlowStatus(statusCode)
}

func (c *FlowClient) post(items []SpoolItem) (int, error) {
	events := make([]FlowEvent, 0, len(items))
	for _, item := range items {
		events = append(events, item.Event)
	}
	body, err := json.Marshal(flowPayload{EventType: FlowEventType, Events: events})
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

func (c *FlowClient) recordSuccess(rejected bool) {
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

func (c *FlowClient) recordFailure(code string, err error) {
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

func classifyFlowTransportError(err error) *flowSendError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &flowSendError{code: "timeout", err: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &flowSendError{code: "timeout", err: err}
	}
	return &flowSendError{code: "network", err: err}
}

func classifyFlowStatus(statusCode int) *flowSendError {
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
	return &flowSendError{code: code, status: statusCode}
}
