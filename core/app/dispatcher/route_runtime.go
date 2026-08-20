package dispatcher

import (
	"context"
	"errors"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	featuredns "github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
)

type RouteRuntimeGeneration struct {
	ID       uint64
	Router   routing.Router
	DNS      featuredns.Client
	Handlers map[string]outbound.Handler
	Default  outbound.Handler
	Cleanup  func() error
}

type routeRuntimeGeneration struct {
	RouteRuntimeGeneration
	refs      int64
	retired   bool
	createdAt time.Time
	retiredAt time.Time
}

type RouteRuntimeManager struct {
	mu       sync.Mutex
	active   *routeRuntimeGeneration
	registry map[uint64]*routeRuntimeGeneration
	retired  map[uint64]*routeRuntimeGeneration
	closed   bool
}

type RouteRuntimeLease struct {
	manager    *RouteRuntimeManager
	generation *routeRuntimeGeneration
	once       sync.Once
}

type RouteRuntimeStats struct {
	ActiveGeneration uint64
	Retired          int
	ActiveLeases     int64
	OldestRetired    time.Duration
}

type routeRuntimeContextKey struct{}

type routeRuntimeContext struct {
	manager    *RouteRuntimeManager
	generation *routeRuntimeGeneration
}

func NewRouteRuntimeManager(initial RouteRuntimeGeneration) *RouteRuntimeManager {
	generation := newRouteRuntimeGeneration(initial, time.Now())
	return &RouteRuntimeManager{
		active:   generation,
		registry: map[uint64]*routeRuntimeGeneration{generation.ID: generation},
		retired:  make(map[uint64]*routeRuntimeGeneration),
	}
}

func newRouteRuntimeGeneration(generation RouteRuntimeGeneration, now time.Time) *routeRuntimeGeneration {
	generation.Handlers = cloneRuntimeHandlers(generation.Handlers)
	return &routeRuntimeGeneration{
		RouteRuntimeGeneration: generation,
		createdAt:              now,
	}
}

func cloneRuntimeHandlers(handlers map[string]outbound.Handler) map[string]outbound.Handler {
	if len(handlers) == 0 {
		return nil
	}
	clone := make(map[string]outbound.Handler, len(handlers))
	for tag, handler := range handlers {
		clone[tag] = handler
	}
	return clone
}

func (m *RouteRuntimeManager) Acquire(ctx context.Context) (*RouteRuntimeLease, error) {
	if m == nil {
		return nil, errors.New("route runtime manager is nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("route runtime manager is closed")
	}

	generation := m.active
	if ctx != nil {
		if inherited, ok := ctx.Value(routeRuntimeContextKey{}).(*routeRuntimeContext); ok && inherited.manager == m {
			registered := m.registry[inherited.generation.ID]
			if registered != inherited.generation {
				return nil, errors.New("inherited route runtime generation is no longer available")
			}
			generation = inherited.generation
		}
	}
	if generation == nil {
		return nil, errors.New("route runtime generation is unavailable")
	}
	generation.refs++
	return &RouteRuntimeLease{manager: m, generation: generation}, nil
}

func (m *RouteRuntimeManager) Publish(next RouteRuntimeGeneration) error {
	if m == nil {
		return errors.New("route runtime manager is nil")
	}
	candidate := newRouteRuntimeGeneration(next, time.Now())
	var cleanup func() error

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("route runtime manager is closed")
	}
	if m.active == nil {
		m.mu.Unlock()
		return errors.New("active route runtime generation is unavailable")
	}
	if candidate.ID <= m.active.ID {
		m.mu.Unlock()
		return errors.New("route runtime generation ID must increase")
	}
	old := m.active
	old.retired = true
	old.retiredAt = time.Now()
	m.retired[old.ID] = old
	m.active = candidate
	m.registry[candidate.ID] = candidate
	if old.refs == 0 {
		cleanup = m.detachRetiredLocked(old)
	}
	m.mu.Unlock()

	runRouteRuntimeCleanup(old.ID, cleanup)
	return nil
}

func (m *RouteRuntimeManager) Close() error {
	if m == nil {
		return nil
	}
	var generationID uint64
	var cleanup func() error
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	if m.active != nil {
		generation := m.active
		generationID = generation.ID
		generation.retired = true
		generation.retiredAt = time.Now()
		m.retired[generation.ID] = generation
		m.active = nil
		if generation.refs == 0 {
			cleanup = m.detachRetiredLocked(generation)
		}
	}
	m.mu.Unlock()
	runRouteRuntimeCleanup(generationID, cleanup)
	return nil
}

func (m *RouteRuntimeManager) detachRetiredLocked(generation *routeRuntimeGeneration) func() error {
	if generation == nil || !generation.retired || generation.refs != 0 {
		return nil
	}
	delete(m.retired, generation.ID)
	delete(m.registry, generation.ID)
	return generation.Cleanup
}

func (m *RouteRuntimeManager) release(generation *routeRuntimeGeneration) {
	if m == nil || generation == nil {
		return
	}
	var cleanup func() error
	m.mu.Lock()
	if registered := m.registry[generation.ID]; registered == generation && generation.refs > 0 {
		generation.refs--
		if generation.retired && generation.refs == 0 {
			cleanup = m.detachRetiredLocked(generation)
		}
	}
	m.mu.Unlock()
	runRouteRuntimeCleanup(generation.ID, cleanup)
}

func runRouteRuntimeCleanup(generationID uint64, cleanup func() error) {
	if cleanup == nil {
		return
	}
	if err := cleanup(); err != nil {
		log.WithField("generation", generationID).WithError(err).Warn("Route runtime cleanup failed")
	}
}

func (m *RouteRuntimeManager) Stats() RouteRuntimeStats {
	if m == nil {
		return RouteRuntimeStats{}
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := RouteRuntimeStats{Retired: len(m.retired)}
	if m.active != nil {
		stats.ActiveGeneration = m.active.ID
	}
	for _, generation := range m.registry {
		stats.ActiveLeases += generation.refs
	}
	for _, generation := range m.retired {
		age := now.Sub(generation.retiredAt)
		if age > stats.OldestRetired {
			stats.OldestRetired = age
		}
	}
	return stats
}

func (l *RouteRuntimeLease) ID() uint64 {
	if l == nil || l.generation == nil {
		return 0
	}
	return l.generation.ID
}

func (l *RouteRuntimeLease) Router() routing.Router {
	if l == nil || l.generation == nil {
		return nil
	}
	return l.generation.Router
}

func (l *RouteRuntimeLease) DNS() featuredns.Client {
	if l == nil || l.generation == nil {
		return nil
	}
	return l.generation.DNS
}

func (l *RouteRuntimeLease) Handler(tag string) outbound.Handler {
	if l == nil || l.generation == nil {
		return nil
	}
	return l.generation.Handlers[tag]
}

func (l *RouteRuntimeLease) DefaultHandler() outbound.Handler {
	if l == nil || l.generation == nil {
		return nil
	}
	return l.generation.Default
}

func (l *RouteRuntimeLease) Context(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil || l.manager == nil || l.generation == nil {
		return ctx
	}
	return context.WithValue(ctx, routeRuntimeContextKey{}, &routeRuntimeContext{
		manager:    l.manager,
		generation: l.generation,
	})
}

func (l *RouteRuntimeLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.manager.release(l.generation)
	})
}
