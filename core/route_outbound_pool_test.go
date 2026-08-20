package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	xraycore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport"
)

func TestRouteOutboundPoolRollsBackPartialCandidate(t *testing.T) {
	manager := newMemoryOutboundManager()
	pool := newRouteOutboundPool(manager, func(config *xraycore.OutboundHandlerConfig) (outbound.Handler, error) {
		if config.Tag == "bad@222" {
			return nil, errors.New("create failed")
		}
		return &memoryOutboundHandler{tag: config.Tag, events: manager.recordEvent}, nil
	})
	_, release, err := pool.Acquire(context.Background(), []*xraycore.OutboundHandlerConfig{
		{Tag: "good@111"},
		{Tag: "bad@222"},
	})
	if err == nil {
		t.Fatal("expected candidate failure")
	}
	if release != nil || manager.GetHandler("good@111") != nil {
		t.Fatal("partial candidate handler was not rolled back")
	}
	if got := manager.eventsSnapshot(); len(got) != 3 || got[0] != "add:good@111" || got[1] != "remove:good@111" || got[2] != "close:good@111" {
		t.Fatalf("rollback events=%v", got)
	}
}

func TestRouteOutboundPoolSharesUntilLastRelease(t *testing.T) {
	manager := newMemoryOutboundManager()
	var creates atomic.Int32
	pool := newRouteOutboundPool(manager, func(config *xraycore.OutboundHandlerConfig) (outbound.Handler, error) {
		creates.Add(1)
		return &memoryOutboundHandler{tag: config.Tag, events: manager.recordEvent}, nil
	})
	configs := []*xraycore.OutboundHandlerConfig{{Tag: "shared@111"}}
	first, releaseFirst, err := pool.Acquire(context.Background(), configs)
	if err != nil {
		t.Fatal(err)
	}
	second, releaseSecond, err := pool.Acquire(context.Background(), configs)
	if err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || first["shared@111"] != second["shared@111"] {
		t.Fatal("equivalent outbound was not shared")
	}
	if err := releaseFirst(); err != nil {
		t.Fatal(err)
	}
	if manager.GetHandler("shared@111") == nil {
		t.Fatal("shared handler was removed before last release")
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
	if manager.GetHandler("shared@111") != nil {
		t.Fatal("handler remained after last release")
	}
	if got := manager.eventsSnapshot(); len(got) != 3 || got[0] != "add:shared@111" || got[1] != "remove:shared@111" || got[2] != "close:shared@111" {
		t.Fatalf("release events=%v", got)
	}
	if err := releaseSecond(); err != nil {
		t.Fatal(err)
	}
	if got := manager.eventsSnapshot(); len(got) != 3 {
		t.Fatalf("idempotent release produced events=%v", got)
	}
}

func TestRouteOutboundPoolRejectsSamePhysicalTagWithDifferentConfig(t *testing.T) {
	manager := newMemoryOutboundManager()
	pool := newRouteOutboundPool(manager, fakeOutboundFactory(nil))
	_, release, err := pool.Acquire(context.Background(), []*xraycore.OutboundHandlerConfig{{Tag: "same@111", Comment: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, candidateRelease, err := pool.Acquire(context.Background(), []*xraycore.OutboundHandlerConfig{{Tag: "same@111", Comment: "different"}}); err == nil || candidateRelease != nil {
		t.Fatal("conflicting physical tag was accepted")
	}
}

type memoryOutboundManager struct {
	mu       sync.Mutex
	handlers map[string]outbound.Handler
	events   []string
}

func newMemoryOutboundManager() *memoryOutboundManager {
	return &memoryOutboundManager{handlers: make(map[string]outbound.Handler)}
}

func (*memoryOutboundManager) Type() interface{} { return outbound.ManagerType() }
func (*memoryOutboundManager) Start() error      { return nil }
func (*memoryOutboundManager) Close() error      { return nil }

func (m *memoryOutboundManager) GetHandler(tag string) outbound.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.handlers[tag]
}

func (m *memoryOutboundManager) GetDefaultHandler() outbound.Handler { return nil }

func (m *memoryOutboundManager) AddHandler(_ context.Context, handler outbound.Handler) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlers[handler.Tag()] != nil {
		return errors.New("duplicate handler")
	}
	m.handlers[handler.Tag()] = handler
	m.events = append(m.events, "add:"+handler.Tag())
	return handler.Start()
}

func (m *memoryOutboundManager) RemoveHandler(_ context.Context, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.handlers, tag)
	m.events = append(m.events, "remove:"+tag)
	return nil
}

func (m *memoryOutboundManager) ListHandlers(context.Context) []outbound.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	handlers := make([]outbound.Handler, 0, len(m.handlers))
	for _, handler := range m.handlers {
		handlers = append(handlers, handler)
	}
	return handlers
}

func (m *memoryOutboundManager) recordEvent(event string) {
	m.mu.Lock()
	m.events = append(m.events, event)
	m.mu.Unlock()
}

func (m *memoryOutboundManager) eventsSnapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

type memoryOutboundHandler struct {
	tag    string
	events func(string)
}

func (*memoryOutboundHandler) Start() error                              { return nil }
func (h *memoryOutboundHandler) Close() error                            { h.events("close:" + h.tag); return nil }
func (h *memoryOutboundHandler) Tag() string                             { return h.tag }
func (*memoryOutboundHandler) Dispatch(context.Context, *transport.Link) {}
func (*memoryOutboundHandler) SenderSettings() *serial.TypedMessage      { return nil }
func (*memoryOutboundHandler) ProxySettings() *serial.TypedMessage       { return nil }

func fakeOutboundFactory(failures map[string]error) outboundFactory {
	return func(config *xraycore.OutboundHandlerConfig) (outbound.Handler, error) {
		if err := failures[config.Tag]; err != nil {
			return nil, err
		}
		return &memoryOutboundHandler{tag: config.Tag, events: func(string) {}}, nil
	}
}
