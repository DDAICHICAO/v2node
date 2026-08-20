package dispatcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/transport"
)

type routeLinkTracker struct {
	lease      *RouteRuntimeLease
	remaining  atomic.Int32
	readerOnce sync.Once
	writerOnce sync.Once
	finishOnce sync.Once
	done       chan struct{}
}

func newRouteTrackedLink(ctx context.Context, link *transport.Link, lease *RouteRuntimeLease) *transport.Link {
	if lease == nil {
		return link
	}
	if link == nil {
		lease.Release()
		return nil
	}
	tracker := &routeLinkTracker{lease: lease, done: make(chan struct{})}
	tracker.remaining.Store(2)
	tracked := &transport.Link{}
	if link.Reader == nil {
		tracker.readerDone()
	} else {
		tracked.Reader = &routeTrackedReader{Reader: link.Reader, tracker: tracker}
	}
	if link.Writer == nil {
		tracker.writerDone()
	} else {
		tracked.Writer = &routeTrackedWriter{Writer: link.Writer, tracker: tracker}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		select {
		case <-ctx.Done():
			tracker.finish()
		case <-tracker.done:
		}
	}()
	return tracked
}

func (t *routeLinkTracker) readerDone() {
	if t == nil {
		return
	}
	t.readerOnce.Do(t.sideDone)
}

func (t *routeLinkTracker) writerDone() {
	if t == nil {
		return
	}
	t.writerOnce.Do(t.sideDone)
}

func (t *routeLinkTracker) sideDone() {
	if t.remaining.Add(-1) == 0 {
		t.finish()
	}
}

func (t *routeLinkTracker) finish() {
	if t == nil {
		return
	}
	t.finishOnce.Do(func() {
		close(t.done)
		t.lease.Release()
	})
}

type routeTrackedReader struct {
	buf.Reader
	tracker *routeLinkTracker
}

func (r *routeTrackedReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if terminalRouteReadError(err) {
		r.tracker.readerDone()
	}
	return mb, err
}

func (r *routeTrackedReader) ReadMultiBufferTimeout(timeout time.Duration) (buf.MultiBuffer, error) {
	timeoutReader, ok := r.Reader.(buf.TimeoutReader)
	if !ok {
		timeoutReader = &buf.TimeoutWrapperReader{Reader: r.Reader}
	}
	mb, err := timeoutReader.ReadMultiBufferTimeout(timeout)
	if terminalRouteReadError(err) {
		r.tracker.readerDone()
	}
	return mb, err
}

func terminalRouteReadError(err error) bool {
	return err != nil && !errors.Is(err, buf.ErrReadTimeout) && !errors.Is(err, buf.ErrNotTimeoutReader)
}

func (r *routeTrackedReader) Interrupt() {
	common.Interrupt(r.Reader)
	r.tracker.readerDone()
}

func (r *routeTrackedReader) Close() error {
	err := common.Close(r.Reader)
	r.tracker.readerDone()
	return err
}

type routeTrackedWriter struct {
	buf.Writer
	tracker *routeLinkTracker
}

func (w *routeTrackedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	err := w.Writer.WriteMultiBuffer(mb)
	if err != nil {
		w.tracker.writerDone()
	}
	return err
}

func (w *routeTrackedWriter) Close() error {
	err := common.Close(w.Writer)
	w.tracker.writerDone()
	return err
}

func (w *routeTrackedWriter) Interrupt() {
	common.Interrupt(w.Writer)
	w.tracker.writerDone()
}
