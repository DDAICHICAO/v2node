package dispatcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/routing"
)

func TestRouteRuntimeRetiresOnlyAfterLastLease(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID:      1,
		Router:  &fakeRuntimeRouter{id: 1},
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	lease, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, Router: &fakeRuntimeRouter{id: 2}}); err != nil {
		t.Fatal(err)
	}
	if cleaned.Load() != 0 {
		t.Fatal("old generation cleaned while lease was active")
	}
	stats := manager.Stats()
	if stats.ActiveGeneration != 2 || stats.Retired != 1 || stats.ActiveLeases != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	lease.Release()
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
	if stats = manager.Stats(); stats.Retired != 0 || stats.ActiveLeases != 0 {
		t.Fatalf("stats after release=%+v", stats)
	}
}

func TestRouteRuntimeContextPinsNestedAcquire(t *testing.T) {
	var cleaned atomic.Int32
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{
		ID:      1,
		Router:  &fakeRuntimeRouter{id: 1},
		Cleanup: func() error { cleaned.Add(1); return nil },
	})
	outer, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := outer.Context(context.Background())
	if err := manager.Publish(RouteRuntimeGeneration{ID: 2, Router: &fakeRuntimeRouter{id: 2}}); err != nil {
		t.Fatal(err)
	}
	nested, err := manager.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if nested.ID() != 1 {
		t.Fatalf("nested generation=%d, want 1", nested.ID())
	}
	fresh, err := manager.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID() != 2 {
		t.Fatalf("fresh generation=%d, want 2", fresh.ID())
	}
	outer.Release()
	if cleaned.Load() != 0 {
		t.Fatal("nested lease was not counted")
	}
	nested.Release()
	if cleaned.Load() != 1 {
		t.Fatalf("cleanup count=%d", cleaned.Load())
	}
	fresh.Release()
}

func TestRouteRuntimeAcquirePublishSeesCompleteGeneration(t *testing.T) {
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, Router: &fakeRuntimeRouter{id: 1}})
	var failed atomic.Bool
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				lease, err := manager.Acquire(context.Background())
				if err != nil {
					failed.Store(true)
					return
				}
				router, ok := lease.Router().(*fakeRuntimeRouter)
				if !ok || router.id != lease.ID() {
					failed.Store(true)
				}
				lease.Release()
			}
		}()
	}
	for id := uint64(2); id <= 100; id++ {
		if err := manager.Publish(RouteRuntimeGeneration{ID: id, Router: &fakeRuntimeRouter{id: id}}); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if failed.Load() {
		t.Fatal("acquire observed a partial generation")
	}
}

func TestRouteRuntimeStatsConcurrentWithAcquireAndPublish(t *testing.T) {
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 1, Router: &fakeRuntimeRouter{id: 1}})
	var failed atomic.Bool
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				stats := manager.Stats()
				if stats.ActiveGeneration == 0 || stats.ActiveLeases < 0 || stats.Retired < 0 {
					failed.Store(true)
					return
				}
			}
		}()
	}
	for id := uint64(2); id <= 100; id++ {
		lease, err := manager.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Publish(RouteRuntimeGeneration{ID: id, Router: &fakeRuntimeRouter{id: id}}); err != nil {
			t.Fatal(err)
		}
		lease.Release()
	}
	close(stop)
	wg.Wait()
	if failed.Load() {
		t.Fatal("concurrent Stats observed an invalid runtime snapshot")
	}
}

func TestRouteRuntimeRejectsNonMonotonicGeneration(t *testing.T) {
	manager := NewRouteRuntimeManager(RouteRuntimeGeneration{ID: 2})
	for _, id := range []uint64{2, 1} {
		if err := manager.Publish(RouteRuntimeGeneration{ID: id}); err == nil {
			t.Fatalf("published non-monotonic generation %d", id)
		}
	}
}

type fakeRuntimeRouter struct {
	id       uint64
	routeTag string
}

func (*fakeRuntimeRouter) Type() interface{} { return routing.RouterType() }
func (*fakeRuntimeRouter) Start() error      { return nil }
func (*fakeRuntimeRouter) Close() error      { return nil }
func (r *fakeRuntimeRouter) PickRoute(ctx routing.Context) (routing.Route, error) {
	if r.routeTag == "" {
		return nil, fmt.Errorf("no route")
	}
	return &fakeRuntimeRoute{Context: ctx, tag: r.routeTag}, nil
}
func (*fakeRuntimeRouter) AddRule(*serial.TypedMessage, bool) error { return nil }
func (*fakeRuntimeRouter) RemoveRule(string) error                  { return nil }
func (*fakeRuntimeRouter) ListRule() []routing.Route                { return nil }

type fakeRuntimeRoute struct {
	routing.Context
	tag string
}

func (*fakeRuntimeRoute) GetOutboundGroupTags() []string { return nil }
func (r *fakeRuntimeRoute) GetOutboundTag() string       { return r.tag }
func (*fakeRuntimeRoute) GetRuleTag() string             { return "test-rule" }
