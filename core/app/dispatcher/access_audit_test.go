package dispatcher

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/wyx2685/v2node/common/accessaudit"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
)

func TestFlowTrafficCountsDirectionsAndEmitsFinalOnce(t *testing.T) {
	startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	now := startedAt.Add(time.Minute)
	reporter := newFlowReporterForTest()
	session := newFlowTrafficSession(flowTrafficMetadata{
		nodeID: 42, nodeTag: "node:42", uid: 145817, uuid: "device-a", sourceIP: "192.0.2.1",
		targetHost: "video.example", targetPort: 443, network: "tcp", inboundTag: "node:42", outboundTag: "proxy-a",
	}, reporter, startedAt, func() time.Time { return now })
	reader := &flowReaderForTest{batches: []buf.MultiBuffer{{buf.FromBytes(make([]byte, 10))}}}
	writer := &flowWriterForTest{}
	link := session.wrapLink(&transport.Link{Reader: reader, Writer: writer})

	dispatchWithFlowTraffic(context.Background(), link, session, func(link *transport.Link) {
		mb, err := link.Reader.ReadMultiBuffer()
		if err != nil {
			t.Fatalf("read upload: %v", err)
		}
		buf.ReleaseMulti(mb)
		if _, err := link.Reader.ReadMultiBuffer(); err != io.EOF {
			t.Fatalf("expected EOF, got %v", err)
		}
		if err := link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes(make([]byte, 20))}); err != nil {
			t.Fatalf("write download: %v", err)
		}
		if err := link.Writer.(interface{ Close() error }).Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
	})

	event := reporter.wait(t)
	if event.SampleType != accessaudit.FlowSampleFinal || event.UploadBytes != 10 || event.DownloadBytes != 20 {
		t.Fatalf("unexpected final event: %#v", event)
	}
	if event.NodeID != 42 || event.UID != 145817 || event.TargetHost != "video.example" || event.OutboundTag != "proxy-a" {
		t.Fatalf("missing routed metadata: %#v", event)
	}
	session.Finish(now.Add(time.Minute))
	time.Sleep(20 * time.Millisecond)
	if reporter.count() != 1 {
		t.Fatalf("expected exactly one final event, got %d", reporter.count())
	}
}

func TestFlowTrafficCheckpointReportsOnlyNewDirectionalBytes(t *testing.T) {
	startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reporter := newFlowReporterForTest()
	session := newFlowTrafficSession(flowTrafficMetadata{nodeID: 1, uid: 2, targetHost: "example.com", targetPort: 443, network: "tcp"}, reporter, startedAt, func() time.Time { return startedAt })
	reader := &flowReaderForTest{batches: []buf.MultiBuffer{
		{buf.FromBytes(make([]byte, 10))},
		{buf.FromBytes(make([]byte, 5))},
	}}
	writer := &flowWriterForTest{}
	link := session.wrapLink(&transport.Link{Reader: reader, Writer: writer})

	firstUpload, _ := link.Reader.ReadMultiBuffer()
	buf.ReleaseMulti(firstUpload)
	_ = link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes(make([]byte, 20))})
	if err := session.Checkpoint(startedAt.Add(5 * time.Minute)); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	secondUpload, _ := link.Reader.ReadMultiBuffer()
	buf.ReleaseMulti(secondUpload)
	_ = link.Writer.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes(make([]byte, 7))})
	session.Finish(startedAt.Add(10 * time.Minute))

	checkpoint := reporter.wait(t)
	final := reporter.wait(t)
	if checkpoint.Sequence != 1 || checkpoint.SampleType != accessaudit.FlowSampleCheckpoint || checkpoint.UploadBytes != 10 || checkpoint.DownloadBytes != 20 {
		t.Fatalf("unexpected checkpoint: %#v", checkpoint)
	}
	if final.Sequence != 2 || final.SampleType != accessaudit.FlowSampleFinal || final.UploadBytes != 5 || final.DownloadBytes != 7 {
		t.Fatalf("unexpected final delta: %#v", final)
	}
}

func TestFlowTrafficAsyncDispatchReturnDoesNotFinalizeEarly(t *testing.T) {
	startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	reporter := newFlowReporterForTest()
	session := newFlowTrafficSession(flowTrafficMetadata{nodeID: 1, uid: 2, targetHost: "example.com", targetPort: 443, network: "tcp"}, reporter, startedAt, func() time.Time { return startedAt.Add(time.Minute) })
	reader := &flowReaderForTest{}
	writer := &flowWriterForTest{}
	link := session.wrapLink(&transport.Link{Reader: reader, Writer: writer})

	dispatchWithFlowTraffic(context.Background(), link, session, func(*transport.Link) {})
	time.Sleep(20 * time.Millisecond)
	if reporter.count() != 0 {
		t.Fatal("handler return must not finalize an asynchronously owned link")
	}
	_, _ = link.Reader.ReadMultiBuffer()
	_ = link.Writer.(interface{ Close() error }).Close()
	event := reporter.wait(t)
	if !event.Completed {
		t.Fatalf("expected final event after both directions terminate: %#v", event)
	}
}

func TestFlowTrafficContextAndPanicFinalize(t *testing.T) {
	t.Run("context cancellation", func(t *testing.T) {
		startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		reporter := newFlowReporterForTest()
		session := newFlowTrafficSession(flowTrafficMetadata{nodeID: 1, uid: 2, targetHost: "example.com", targetPort: 443, network: "tcp"}, reporter, startedAt, func() time.Time { return startedAt.Add(time.Minute) })
		ctx, cancel := context.WithCancel(context.Background())
		dispatchWithFlowTraffic(ctx, &transport.Link{Reader: &flowReaderForTest{}, Writer: &flowWriterForTest{}}, session, func(*transport.Link) {})
		cancel()
		if event := reporter.wait(t); !event.Completed {
			t.Fatalf("expected context final event: %#v", event)
		}
	})

	t.Run("panic", func(t *testing.T) {
		startedAt := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
		reporter := newFlowReporterForTest()
		session := newFlowTrafficSession(flowTrafficMetadata{nodeID: 1, uid: 2, targetHost: "example.com", targetPort: 443, network: "tcp"}, reporter, startedAt, func() time.Time { return startedAt.Add(time.Minute) })
		func() {
			defer func() {
				if recovered := recover(); recovered != "boom" {
					t.Fatalf("expected original panic, got %#v", recovered)
				}
			}()
			dispatchWithFlowTraffic(context.Background(), &transport.Link{Reader: &flowReaderForTest{}, Writer: &flowWriterForTest{}}, session, func(*transport.Link) {
				panic("boom")
			})
		}()
		if event := reporter.wait(t); !event.Completed {
			t.Fatalf("expected panic final event: %#v", event)
		}
	})
}

type flowReporterForTest struct {
	mu     sync.Mutex
	events []accessaudit.FlowEvent
	ch     chan accessaudit.FlowEvent
}

func newFlowReporterForTest() *flowReporterForTest {
	return &flowReporterForTest{ch: make(chan accessaudit.FlowEvent, 8)}
}

func (r *flowReporterForTest) Report(event accessaudit.FlowEvent) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	r.ch <- event
	return nil
}

func (r *flowReporterForTest) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *flowReporterForTest) wait(t *testing.T) accessaudit.FlowEvent {
	t.Helper()
	select {
	case event := <-r.ch:
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for flow event")
		return accessaudit.FlowEvent{}
	}
}

type flowReaderForTest struct {
	batches []buf.MultiBuffer
	index   int
}

func (r *flowReaderForTest) ReadMultiBuffer() (buf.MultiBuffer, error) {
	if r.index >= len(r.batches) {
		return nil, io.EOF
	}
	batch := r.batches[r.index]
	r.index++
	return batch, nil
}

func (r *flowReaderForTest) Interrupt() {}

type flowWriterForTest struct{}

func (w *flowWriterForTest) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return nil
}

func (w *flowWriterForTest) Close() error { return nil }
func (w *flowWriterForTest) Interrupt()   {}

func TestExtractNodeIDFromInboundTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want uint32
	}{
		{name: "standard tag", tag: "[https://panel.example.com]-shadowsocks:514", want: 514},
		{name: "url contains port", tag: "[https://panel.example.com:8443]-vless:9", want: 9},
		{name: "empty tag", tag: "", want: 0},
		{name: "malformed tag", tag: "sntp-eclipse", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractNodeIDFromInboundTag(tt.tag); got != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, got)
			}
		})
	}
}
