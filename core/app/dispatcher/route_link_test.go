package dispatcher

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
)

func TestRouteLinkReleasesOnlyAfterBothSidesEnd(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID:      1,
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tracked := newRouteTrackedLink(context.Background(), &transport.Link{
		Reader: &lifecycleReader{},
		Writer: &lifecycleWriter{},
	}, lease)
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2}); err != nil {
		t.Fatal(err)
	}
	common.Interrupt(tracked.Reader)
	if cleaned.Load() != 0 {
		t.Fatal("generation cleaned after only reader ended")
	}
	if err := common.Close(tracked.Writer); err != nil {
		t.Fatal(err)
	}
	waitForRuntimeRetiredCount(t, manager, 0)
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
	common.Interrupt(tracked.Reader)
	_ = common.Close(tracked.Writer)
	if cleaned.Load() != 1 {
		t.Fatalf("duplicate close cleaned %d times", cleaned.Load())
	}
}

func TestRouteLinkContextCancellationReleasesLease(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID:      1,
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_ = newRouteTrackedLink(ctx, &transport.Link{Reader: &lifecycleReader{}, Writer: &lifecycleWriter{}}, lease)
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2}); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitForRuntimeRetiredCount(t, manager, 0)
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
}

func TestRouteLinkReadTimeoutIsNotTerminal(t *testing.T) {
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1})
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader := &timeoutRuntimeReader{}
	tracked := newRouteTrackedLink(context.Background(), &transport.Link{
		Reader: reader,
		Writer: &lifecycleWriter{},
	}, lease)
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := tracked.Reader.(buf.TimeoutReader).ReadMultiBufferTimeout(time.Millisecond); err != buf.ErrReadTimeout {
		t.Fatalf("timeout error=%v", err)
	}
	_ = common.Close(tracked.Writer)
	if manager.Stats().Retired != 1 {
		t.Fatal("read timeout ended the reader side")
	}
	common.Interrupt(tracked.Reader)
	waitForRuntimeRetiredCount(t, manager, 0)
}

func TestRoutedDispatchKeepsGenerationUntilLinkEnds(t *testing.T) {
	oldHandler := newRecordingRuntimeHandler("old")
	newHandler := newRecordingRuntimeHandler("new")
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(runtimeHandlerGeneration(1, oldHandler, nil, func() error {
		cleaned.Add(1)
		return nil
	}))
	d := &DefaultDispatcher{}
	d.BindRouteRuntime(manager)
	d.routedDispatch(runtimeRoutingContext(), &transport.Link{
		Reader: &lifecycleReader{},
		Writer: &lifecycleWriter{},
	}, runtimeTestDestination())

	var dispatched *transport.Link
	select {
	case dispatched = <-oldHandler.links:
	case <-time.After(time.Second):
		t.Fatal("old handler was not dispatched")
	}
	if err := manager.Publish(runtimeHandlerGeneration(2, newHandler, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if stats := manager.Stats(); stats.Retired != 1 || stats.ActiveLeases != 1 {
		t.Fatalf("old generation was not retained: %+v", stats)
	}
	common.Interrupt(dispatched.Reader)
	_ = common.Close(dispatched.Writer)
	waitForRuntimeRetiredCount(t, manager, 0)
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
}

func TestRoutedDispatchUsesGenerationForForcedRouterAndDefaultHandlers(t *testing.T) {
	tests := []struct {
		name      string
		configure func(context.Context, *fakeRuntimeRouter) context.Context
		want      string
	}{
		{name: "default", configure: func(ctx context.Context, _ *fakeRuntimeRouter) context.Context { return ctx }, want: "default"},
		{name: "forced", configure: func(ctx context.Context, _ *fakeRuntimeRouter) context.Context {
			return session.SetForcedOutboundTagToContext(ctx, "forced")
		}, want: "forced"},
		{name: "router", configure: func(ctx context.Context, router *fakeRuntimeRouter) context.Context {
			router.routeTag = "routed"
			return ctx
		}, want: "routed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			router := &fakeRuntimeRouter{id: 1}
			handlers := map[string]*recordingRuntimeHandler{
				"default": newRecordingRuntimeHandler("default"),
				"forced":  newRecordingRuntimeHandler("forced"),
				"routed":  newRecordingRuntimeHandler("routed"),
			}
			generationHandlers := make(map[string]outbound.Handler, len(handlers))
			for tag, handler := range handlers {
				generationHandlers[tag] = handler
			}
			manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
				ID:       1,
				Router:   router,
				Handlers: generationHandlers,
				Default:  handlers["default"],
			})
			d := &DefaultDispatcher{}
			d.BindRouteRuntime(manager)
			ctx := tc.configure(runtimeRoutingContext(), router)
			d.routedDispatch(ctx, &transport.Link{Reader: &lifecycleReader{}, Writer: &lifecycleWriter{}}, runtimeTestDestination())
			select {
			case link := <-handlers[tc.want].links:
				common.Interrupt(link.Reader)
				_ = common.Close(link.Writer)
			case <-time.After(time.Second):
				t.Fatalf("%s handler was not selected", tc.want)
			}
			for tag, handler := range handlers {
				want := int32(0)
				if tag == tc.want {
					want = 1
				}
				if got := handler.calls.Load(); got != want {
					t.Fatalf("handler %s calls=%d, want %d", tag, got, want)
				}
			}
		})
	}
}

func TestConcurrentRoutePublishAndDispatch(t *testing.T) {
	first := newClosingRuntimeHandler("generation-1")
	manager := NewRouteRuntimeManager(runtimeHandlerGeneration(1, first, nil, nil))
	d := &DefaultDispatcher{}
	d.BindRouteRuntime(manager)

	const connections = 1000
	var wait sync.WaitGroup
	wait.Add(connections)
	for i := 0; i < connections; i++ {
		go func() {
			defer wait.Done()
			d.routedDispatch(runtimeRoutingContext(), &transport.Link{
				Reader: &lifecycleReader{},
				Writer: &lifecycleWriter{},
			}, runtimeTestDestination())
		}()
		if i%20 == 0 {
			id := uint64(i/20 + 2)
			handler := newClosingRuntimeHandler("generation")
			if err := manager.Publish(runtimeHandlerGeneration(id, handler, nil, nil)); err != nil {
				t.Fatal(err)
			}
		}
	}
	wait.Wait()
	waitForRuntimeRetiredCount(t, manager, 0)
	if stats := manager.Stats(); stats.ActiveLeases != 0 {
		t.Fatalf("active leases=%d", stats.ActiveLeases)
	}
}

type recordingRuntimeHandler struct {
	tag   string
	calls atomic.Int32
	links chan *transport.Link
}

func newRecordingRuntimeHandler(tag string) *recordingRuntimeHandler {
	return &recordingRuntimeHandler{tag: tag, links: make(chan *transport.Link, 1024)}
}

func (*recordingRuntimeHandler) Start() error                         { return nil }
func (*recordingRuntimeHandler) Close() error                         { return nil }
func (h *recordingRuntimeHandler) Tag() string                        { return h.tag }
func (*recordingRuntimeHandler) SenderSettings() *serial.TypedMessage { return nil }
func (*recordingRuntimeHandler) ProxySettings() *serial.TypedMessage  { return nil }
func (h *recordingRuntimeHandler) Dispatch(_ context.Context, link *transport.Link) {
	h.calls.Add(1)
	h.links <- link
}

type closingRuntimeHandler struct {
	*recordingRuntimeHandler
}

func newClosingRuntimeHandler(tag string) *closingRuntimeHandler {
	return &closingRuntimeHandler{recordingRuntimeHandler: newRecordingRuntimeHandler(tag)}
}

func (h *closingRuntimeHandler) Dispatch(_ context.Context, link *transport.Link) {
	h.calls.Add(1)
	common.Interrupt(link.Reader)
	_ = common.Close(link.Writer)
}

func runtimeHandlerGeneration(id uint64, handler outbound.Handler, router *fakeRuntimeRouter, cleanup func() error) RouteRuntimeGeneration {
	generation := RouteRuntimeGeneration{
		ID:       id,
		Handlers: map[string]outbound.Handler{handler.Tag(): handler},
		Default:  handler,
		Cleanup:  cleanup,
	}
	if router != nil {
		generation.Router = router
	}
	return generation
}

func runtimeRoutingContext() context.Context {
	return session.ContextWithOutbounds(context.Background(), []*session.Outbound{{}})
}

func runtimeTestDestination() xnet.Destination {
	return xnet.TCPDestination(xnet.ParseAddress("example.com"), 443)
}

func waitForRuntimeRetiredCount(t *testing.T, manager *RouteRuntimeManager, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Stats().Retired == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("retired=%d, want %d", manager.Stats().Retired, want)
}

var _ buf.Reader = (*lifecycleReader)(nil)

type timeoutRuntimeReader struct{}

func (*timeoutRuntimeReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return nil, io.EOF
}

func (*timeoutRuntimeReader) ReadMultiBufferTimeout(time.Duration) (buf.MultiBuffer, error) {
	return nil, buf.ErrReadTimeout
}

func (*timeoutRuntimeReader) Interrupt() {}
