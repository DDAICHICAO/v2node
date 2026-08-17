package dispatcher

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/buf"
)

type lifecycleWriter struct {
	closed atomic.Bool
}

func (w *lifecycleWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return nil
}

func (w *lifecycleWriter) Close() error {
	w.closed.Store(true)
	return nil
}

type lifecycleReader struct {
	interrupted atomic.Bool
}

func (r *lifecycleReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return nil, io.EOF
}

func (r *lifecycleReader) Interrupt() {
	r.interrupted.Store(true)
}

func TestLinkRegistryRejectsInactiveUserAndReleasesLink(t *testing.T) {
	registry := NewLinkRegistry()
	writer := &lifecycleWriter{}
	reader := &lifecycleReader{}

	managed, ok := registry.RegisterLink("node|inactive", writer, reader, "192.0.2.1")
	if ok || managed != nil {
		t.Fatal("inactive user registration was accepted")
	}
	if !writer.closed.Load() || !reader.interrupted.Load() {
		t.Fatal("rejected link was not released")
	}
}

func TestLinkRegistryDeactivateClosesAndPreventsRevival(t *testing.T) {
	registry := NewLinkRegistry()
	const user = "node|active"
	registry.ActivateUser(user)
	writer := &lifecycleWriter{}
	reader := &lifecycleReader{}
	if _, ok := registry.RegisterLink(user, writer, reader, "192.0.2.2"); !ok {
		t.Fatal("active user registration was rejected")
	}

	if got := registry.DeactivateUser(user); got != 1 {
		t.Fatalf("closed=%d want=1", got)
	}
	if !writer.closed.Load() || !reader.interrupted.Load() {
		t.Fatal("deactivated link remained open")
	}

	lateWriter := &lifecycleWriter{}
	lateReader := &lifecycleReader{}
	if _, ok := registry.RegisterLink(user, lateWriter, lateReader, "192.0.2.3"); ok {
		t.Fatal("deactivated user revived its slot")
	}
	if !lateWriter.closed.Load() || !lateReader.interrupted.Load() {
		t.Fatal("late rejected link was not released")
	}
	if got := registry.DeactivateUser(user); got != 0 {
		t.Fatalf("repeated deactivate closed=%d want=0", got)
	}
}

func TestLinkRegistryConcurrentRegistrationUsesOneManager(t *testing.T) {
	registry := NewLinkRegistry()
	const user = "node|concurrent"
	registry.ActivateUser(user)
	const count = 32
	start := make(chan struct{})
	managers := make(chan *LinkManager, count)
	var wait sync.WaitGroup
	wait.Add(count)
	for i := 0; i < count; i++ {
		go func() {
			defer wait.Done()
			<-start
			managed, ok := registry.RegisterLink(user, &lifecycleWriter{}, &lifecycleReader{}, "192.0.2.10")
			if !ok {
				return
			}
			managers <- managed.manager
		}()
	}
	close(start)
	wait.Wait()
	close(managers)
	var first *LinkManager
	registered := 0
	for manager := range managers {
		registered++
		if first == nil {
			first = manager
		}
		if manager != first {
			t.Fatal("concurrent registration used multiple managers")
		}
	}
	if registered != count {
		t.Fatalf("registered=%d want=%d", registered, count)
	}
	if closed := registry.DeactivateUser(user); closed != count {
		t.Fatalf("closed=%d want=%d", closed, count)
	}
}

func TestLinkRegistryCloseUserIPOnlyClosesMatchingLinks(t *testing.T) {
	registry := NewLinkRegistry()
	const user = "node|blocked-ip"
	registry.ActivateUser(user)
	blockedWriter := &lifecycleWriter{}
	blockedReader := &lifecycleReader{}
	allowedWriter := &lifecycleWriter{}
	allowedReader := &lifecycleReader{}
	_, _ = registry.RegisterLink(user, blockedWriter, blockedReader, "192.0.2.11")
	_, _ = registry.RegisterLink(user, allowedWriter, allowedReader, "192.0.2.12")

	if closed := registry.CloseUserIP(user, "192.0.2.11"); closed != 1 {
		t.Fatalf("closed=%d want=1", closed)
	}
	if !blockedWriter.closed.Load() || !blockedReader.interrupted.Load() {
		t.Fatal("matching link remained open")
	}
	if allowedWriter.closed.Load() || allowedReader.interrupted.Load() {
		t.Fatal("nonmatching link was closed")
	}
	if closed := registry.DeactivateUser(user); closed != 1 {
		t.Fatalf("remaining closed=%d want=1", closed)
	}
}

func TestLinkRegistryDeactivateIncludesRegistrationAlreadyInsideLifecycle(t *testing.T) {
	registry := NewLinkRegistry()
	const user = "node|barrier"
	registry.ActivateUser(user)

	registry.mu.RLock()
	manager := registry.users[user]
	writer := &lifecycleWriter{}
	reader := &lifecycleReader{}
	managed := &ManagedWriter{writer: writer, manager: manager, source: "192.0.2.4"}
	started := make(chan struct{})
	done := make(chan int, 1)
	go func() {
		close(started)
		done <- registry.DeactivateUser(user)
	}()
	<-started
	select {
	case <-done:
		registry.mu.RUnlock()
		t.Fatal("deactivate crossed an active registry read boundary")
	default:
	}
	manager.AddLink(managed, reader)
	registry.mu.RUnlock()

	if got := <-done; got != 1 {
		t.Fatalf("closed=%d want=1", got)
	}
	if !writer.closed.Load() || !reader.interrupted.Load() {
		t.Fatal("in-flight registration escaped deactivation")
	}
}

func TestLinkRegistryReactivationUsesIndependentManager(t *testing.T) {
	registry := NewLinkRegistry()
	const user = "node|reauthorized"
	registry.ActivateUser(user)
	oldWriter := &lifecycleWriter{}
	oldReader := &lifecycleReader{}
	oldManaged, ok := registry.RegisterLink(user, oldWriter, oldReader, "192.0.2.5")
	if !ok {
		t.Fatal("initial registration was rejected")
	}
	registry.DeactivateUser(user)

	registry.ActivateUser(user)
	newWriter := &lifecycleWriter{}
	newReader := &lifecycleReader{}
	if _, ok := registry.RegisterLink(user, newWriter, newReader, "192.0.2.6"); !ok {
		t.Fatal("reauthorized registration was rejected")
	}
	_ = oldManaged.Close()
	if got := registry.DeactivateUser(user); got != 1 {
		t.Fatalf("new lifecycle closed=%d want=1", got)
	}
	if !newWriter.closed.Load() || !newReader.interrupted.Load() {
		t.Fatal("old lifecycle callback corrupted the new manager")
	}
}
