package dispatcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyx2685/v2node/common/accessaudit"
	"github.com/wyx2685/v2node/limiter"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

type flowTrafficReporter interface {
	Report(accessaudit.FlowEvent) error
}

type defaultFlowTrafficReporter struct{}

func (defaultFlowTrafficReporter) Report(event accessaudit.FlowEvent) error {
	return accessaudit.ReportFlow(event)
}

type flowTrafficMetadata struct {
	nodeID      uint32
	nodeTag     string
	uid         uint64
	uuid        string
	sourceIP    string
	targetHost  string
	targetPort  uint16
	network     string
	inboundTag  string
	outboundTag string
}

type flowTrafficSession struct {
	metadata  flowTrafficMetadata
	reporter  flowTrafficReporter
	sessionID string
	startedAt time.Time
	now       func() time.Time

	sequence      atomic.Uint32
	uploaded      atomic.Uint64
	downloaded    atomic.Uint64
	reportedUp    atomic.Uint64
	reportedDown  atomic.Uint64
	finished      atomic.Bool
	emitMu        sync.Mutex
	intervalStart time.Time
	finishOnce    sync.Once
	lifecycleOnce sync.Once
	uploadDone    chan struct{}
	downloadDone  chan struct{}
	done          chan struct{}
	uploadOnce    sync.Once
	downloadOnce  sync.Once
}

func newFlowTrafficSession(metadata flowTrafficMetadata, reporter flowTrafficReporter, startedAt time.Time, now func() time.Time) *flowTrafficSession {
	if reporter == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	if startedAt.IsZero() {
		startedAt = now()
	}
	return &flowTrafficSession{
		metadata:      metadata,
		reporter:      reporter,
		sessionID:     newFlowSessionID(),
		startedAt:     startedAt,
		intervalStart: startedAt,
		now:           now,
		uploadDone:    make(chan struct{}),
		downloadDone:  make(chan struct{}),
		done:          make(chan struct{}),
	}
}

func newRoutedFlowTrafficSession(ctx context.Context, destination net.Destination, outboundTag string) *flowTrafficSession {
	if !accessaudit.FlowTrafficEnabled() {
		return nil
	}
	inbound := session.InboundFromContext(ctx)
	if inbound == nil || inbound.User == nil || inbound.User.Email == "" {
		return nil
	}
	uid, uuid := flowTrafficUserIdentity(inbound.Tag, inbound.User.Email)
	nodeID := extractNodeIDFromInboundTag(inbound.Tag)
	network := accessAuditNetwork(destination)
	if uid == 0 || uuid == "" || nodeID == 0 || network == "" || destination.Port == 0 {
		return nil
	}
	return newFlowTrafficSession(flowTrafficMetadata{
		nodeID:      nodeID,
		nodeTag:     inbound.Tag,
		uid:         uint64(uid),
		uuid:        uuid,
		sourceIP:    stringsTrimMappedIP(inbound.Source.Address.IP().String()),
		targetHost:  destination.Address.String(),
		targetPort:  uint16(destination.Port),
		network:     network,
		inboundTag:  inbound.Tag,
		outboundTag: outboundTag,
	}, defaultFlowTrafficReporter{}, time.Now(), time.Now)
}

func flowTrafficUserIdentity(inboundTag, userEmail string) (int, string) {
	uuid := extractUUIDFromUserEmail(userEmail)
	if uuid == "" {
		return 0, ""
	}
	limit, err := limiter.GetLimiter(inboundTag)
	if err != nil {
		return 0, uuid
	}
	value, ok := limit.UserLimitInfo.Load(userEmail)
	if !ok {
		return 0, uuid
	}
	info, ok := value.(*limiter.UserLimitInfo)
	if !ok {
		return 0, uuid
	}
	return info.UID, uuid
}

func stringsTrimMappedIP(value string) string {
	const prefix = "::ffff:"
	if len(value) >= len(prefix) && value[:len(prefix)] == prefix {
		return value[len(prefix):]
	}
	return value
}

func (s *flowTrafficSession) wrapLink(link *transport.Link) *transport.Link {
	if s == nil || link == nil {
		return link
	}
	return &transport.Link{
		Reader: &flowTrafficReader{Reader: link.Reader, session: s},
		Writer: &flowTrafficWriter{Writer: link.Writer, session: s},
	}
}

func (s *flowTrafficSession) Checkpoint(at time.Time) error {
	if s == nil || s.finished.Load() {
		return nil
	}
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if s.finished.Load() {
		return nil
	}
	return s.emit(at, accessaudit.FlowSampleCheckpoint, false)
}

func (s *flowTrafficSession) Finish(at time.Time) {
	if s == nil {
		return
	}
	s.finishOnce.Do(func() {
		s.finished.Store(true)
		close(s.done)
		s.emitMu.Lock()
		defer s.emitMu.Unlock()
		_ = s.emit(at, accessaudit.FlowSampleFinal, true)
	})
}

func (s *flowTrafficSession) emit(at time.Time, sampleType string, includeZero bool) error {
	if at.IsZero() {
		at = s.now()
	}
	uploaded := s.uploaded.Load()
	downloaded := s.downloaded.Load()
	uploadDelta := uploaded - s.reportedUp.Load()
	downloadDelta := downloaded - s.reportedDown.Load()
	if !includeZero && uploadDelta == 0 && downloadDelta == 0 {
		return nil
	}
	sequence := s.sequence.Load() + 1
	duration := at.Sub(s.intervalStart)
	if duration < 0 {
		duration = 0
	}
	event := accessaudit.FlowEvent{
		SessionID:         s.sessionID,
		Sequence:          sequence,
		SampleType:        sampleType,
		EventTime:         at,
		IntervalStartedAt: s.intervalStart,
		NodeID:            s.metadata.nodeID,
		NodeTag:           s.metadata.nodeTag,
		UID:               s.metadata.uid,
		UUID:              s.metadata.uuid,
		SourceIP:          s.metadata.sourceIP,
		TargetHost:        s.metadata.targetHost,
		TargetPort:        s.metadata.targetPort,
		Network:           s.metadata.network,
		InboundTag:        s.metadata.inboundTag,
		OutboundTag:       s.metadata.outboundTag,
		UploadBytes:       uploadDelta,
		DownloadBytes:     downloadDelta,
		DurationMS:        uint64(duration / time.Millisecond),
	}
	if err := event.Normalize(at); err != nil {
		return err
	}
	if err := s.reporter.Report(event); err != nil {
		return err
	}
	s.sequence.Store(sequence)
	s.reportedUp.Store(uploaded)
	s.reportedDown.Store(downloaded)
	s.intervalStart = at
	return nil
}

func (s *flowTrafficSession) startLifecycle(ctx context.Context) {
	if s == nil {
		return
	}
	s.lifecycleOnce.Do(func() {
		go func() {
			checkpointInterval := accessaudit.FlowCheckpointInterval()
			var checkpoint <-chan time.Time
			var ticker *time.Ticker
			if checkpointInterval > 0 {
				ticker = time.NewTicker(checkpointInterval)
				checkpoint = ticker.C
				defer ticker.Stop()
			}
			uploadDone := s.uploadDone
			downloadDone := s.downloadDone
			for {
				select {
				case <-ctx.Done():
					s.Finish(s.now())
					return
				case <-uploadDone:
					uploadDone = nil
					if downloadDone == nil {
						s.Finish(s.now())
						return
					}
				case <-downloadDone:
					downloadDone = nil
					if uploadDone == nil {
						s.Finish(s.now())
						return
					}
				case at := <-checkpoint:
					_ = s.Checkpoint(at)
				case <-s.done:
					return
				}
			}
		}()
	})
}

func dispatchWithFlowTraffic(ctx context.Context, link *transport.Link, session *flowTrafficSession, dispatch func(*transport.Link)) {
	if session == nil {
		dispatch(link)
		return
	}
	session.startLifecycle(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			session.Finish(session.now())
			panic(recovered)
		}
	}()
	dispatch(link)
}

type flowTrafficReader struct {
	buf.Reader
	session *flowTrafficSession
}

func (r *flowTrafficReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if r.session != nil {
		r.session.uploaded.Add(uint64(mb.Len()))
		if err != nil {
			r.session.uploadOnce.Do(func() { close(r.session.uploadDone) })
		}
	}
	return mb, err
}

func (r *flowTrafficReader) ReadMultiBufferTimeout(timeout time.Duration) (buf.MultiBuffer, error) {
	timeoutReader, ok := r.Reader.(buf.TimeoutReader)
	if !ok {
		timeoutReader = &buf.TimeoutWrapperReader{Reader: r.Reader}
	}
	mb, err := timeoutReader.ReadMultiBufferTimeout(timeout)
	if r.session != nil {
		r.session.uploaded.Add(uint64(mb.Len()))
		if err != nil {
			r.session.uploadOnce.Do(func() { close(r.session.uploadDone) })
		}
	}
	return mb, err
}

func (r *flowTrafficReader) Interrupt() {
	if r.session != nil {
		r.session.uploadOnce.Do(func() { close(r.session.uploadDone) })
	}
	common.Interrupt(r.Reader)
}

func (r *flowTrafficReader) Close() error {
	if r.session != nil {
		r.session.uploadOnce.Do(func() { close(r.session.uploadDone) })
	}
	return common.Close(r.Reader)
}

type flowTrafficWriter struct {
	buf.Writer
	session *flowTrafficSession
}

func (w *flowTrafficWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	size := mb.Len()
	err := w.Writer.WriteMultiBuffer(mb)
	if w.session != nil {
		if err == nil {
			w.session.downloaded.Add(uint64(size))
		} else {
			w.session.downloadOnce.Do(func() { close(w.session.downloadDone) })
		}
	}
	return err
}

func (w *flowTrafficWriter) Close() error {
	if w.session != nil {
		w.session.downloadOnce.Do(func() { close(w.session.downloadDone) })
	}
	return common.Close(w.Writer)
}

func (w *flowTrafficWriter) Interrupt() {
	if w.session != nil {
		w.session.downloadOnce.Do(func() { close(w.session.downloadDone) })
	}
	common.Interrupt(w.Writer)
}

var fallbackFlowSessionSequence atomic.Uint64

func newFlowSessionID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), fallbackFlowSessionSequence.Add(1))
}
