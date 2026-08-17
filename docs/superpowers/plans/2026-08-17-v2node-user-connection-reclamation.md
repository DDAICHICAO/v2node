# v2node User Connection Reclamation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make an expired, traffic-exhausted, banned, or suspended user immediately lose every existing v2node forwarding connection, while preventing an authenticated in-flight request from recreating the user's connection slot.

**Architecture:** Replace the dispatcher-owned `sync.Map` with an authoritative `LinkRegistry`: only `V2Core.AddUsers` activates a user, both dispatcher paths may only register into an active slot, and `V2Core.DelUsers` removes the slot before any potentially slow credential cleanup. Registry and manager locks establish one lifecycle boundary; actual writer close and reader interrupt happen outside the registry lock.

**Tech Stack:** Go 1.26, Xray `buf.Reader`/`buf.Writer`, `sync.RWMutex`, `sync/atomic`, standard `testing`, `go vet`, Linux race detector.

---

## File map

- `core/app/dispatcher/linkmanager.go`: own `ManagedWriter`, per-user `LinkManager`, and the new authoritative `LinkRegistry` lifecycle API.
- `core/app/dispatcher/linkmanager_test.go`: prove fail-closed registration, atomic deactivation, deterministic registration/deactivation ordering, and clean reauthorization.
- `core/app/dispatcher/default.go`: initialize the registry and route both existing authenticated connection-registration paths through it.
- `core/user.go`: activate registry slots only after credential installation succeeds; deactivate the complete delete batch before cleaning credentials, UID mappings, counters, and limiter state through the existing caller chain.
- `core/user_test.go`: exercise idempotent credential installation, partial-add rollback, fail-closed deletion, and stale-credential reauthorization with a fake `proxy.UserManager`.
- `LESSONS_LEARNED.md`: replace the diagnosis-only status with the implemented boundary, exact verification evidence, and remaining Linux/production checks.

### Task 1: Introduce the authoritative `LinkRegistry`

**Files:**
- Create: `core/app/dispatcher/linkmanager_test.go`
- Modify: `core/app/dispatcher/linkmanager.go`

- [ ] **Step 1: Write the failing registry contract tests**

Create `core/app/dispatcher/linkmanager_test.go` with deterministic close doubles and lifecycle tests:

```go
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
```

- [ ] **Step 2: Run the registry tests and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run '^TestLinkRegistry' -count=1
```

Expected: compilation fails because `NewLinkRegistry`, `RegisterLink`, `ActivateUser`, and `DeactivateUser` do not exist.

- [ ] **Step 3: Replace `linkmanager.go` with the lifecycle implementation**

Use this complete implementation:

```go
package dispatcher

import (
	"sync"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

type ManagedWriter struct {
	writer  buf.Writer
	manager *LinkManager
	source  string
}

func (w *ManagedWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	return w.writer.WriteMultiBuffer(mb)
}

func (w *ManagedWriter) Close() error {
	w.manager.RemoveWriter(w)
	return common.Close(w.writer)
}

type LinkManager struct {
	links map[*ManagedWriter]buf.Reader
	mu    sync.RWMutex
}

func newLinkManager() *LinkManager {
	return &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
}

func (m *LinkManager) AddLink(writer *ManagedWriter, reader buf.Reader) {
	m.mu.Lock()
	m.links[writer] = reader
	m.mu.Unlock()
}

func (m *LinkManager) RemoveWriter(writer *ManagedWriter) {
	m.mu.Lock()
	delete(m.links, writer)
	m.mu.Unlock()
}

func (m *LinkManager) CloseAll() int {
	return m.closeMatched(func(*ManagedWriter) bool { return true })
}

func (m *LinkManager) CloseByIP(ip string) int {
	return m.closeMatched(func(w *ManagedWriter) bool { return w.source == ip })
}

func (m *LinkManager) closeMatched(match func(*ManagedWriter) bool) int {
	var writers []*ManagedWriter
	var readers []buf.Reader
	m.mu.Lock()
	for writer, reader := range m.links {
		if match(writer) {
			writers = append(writers, writer)
			readers = append(readers, reader)
			delete(m.links, writer)
		}
	}
	m.mu.Unlock()
	for i, writer := range writers {
		_ = common.Close(writer.writer)
		_ = common.Interrupt(readers[i])
	}
	return len(writers)
}

type LinkRegistry struct {
	mu    sync.RWMutex
	users map[string]*LinkManager
}

func NewLinkRegistry() *LinkRegistry {
	return &LinkRegistry{users: make(map[string]*LinkManager)}
}

func (r *LinkRegistry) ActivateUser(user string) {
	if r == nil || user == "" {
		return
	}
	r.mu.Lock()
	if _, exists := r.users[user]; !exists {
		r.users[user] = newLinkManager()
	}
	r.mu.Unlock()
}

func (r *LinkRegistry) RegisterLink(user string, writer buf.Writer, reader buf.Reader, source string) (*ManagedWriter, bool) {
	if r == nil || user == "" {
		_ = common.Close(writer)
		_ = common.Interrupt(reader)
		return nil, false
	}
	r.mu.RLock()
	manager, exists := r.users[user]
	if !exists {
		r.mu.RUnlock()
		_ = common.Close(writer)
		_ = common.Interrupt(reader)
		return nil, false
	}
	managed := &ManagedWriter{writer: writer, manager: manager, source: source}
	manager.AddLink(managed, reader)
	r.mu.RUnlock()
	return managed, true
}

func (r *LinkRegistry) DeactivateUser(user string) int {
	if r == nil || user == "" {
		return 0
	}
	r.mu.Lock()
	manager, exists := r.users[user]
	if exists {
		delete(r.users, user)
	}
	r.mu.Unlock()
	if !exists {
		return 0
	}
	return manager.CloseAll()
}

func (r *LinkRegistry) CloseUserIP(user, ip string) int {
	if r == nil || user == "" || ip == "" {
		return 0
	}
	r.mu.RLock()
	manager, exists := r.users[user]
	r.mu.RUnlock()
	if !exists {
		return 0
	}
	return manager.CloseByIP(ip)
}
```

- [ ] **Step 4: Format and verify GREEN**

Run:

```powershell
gofmt -w core/app/dispatcher/linkmanager.go core/app/dispatcher/linkmanager_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run '^TestLinkRegistry' -count=1
```

Expected: all six `TestLinkRegistry*` tests pass.

- [ ] **Step 5: Commit the isolated lifecycle primitive**

```powershell
git add core/app/dispatcher/linkmanager.go core/app/dispatcher/linkmanager_test.go
git commit -m "修复：建立用户连接生命周期注册表" -m "只有已激活用户才能登记连接，停用操作原子摘除连接槽并在锁外硬断全部读写链路。"
```

### Task 2: Route both dispatcher entrypoints through the registry

**Files:**
- Modify: `core/app/dispatcher/linkmanager_test.go`
- Modify: `core/app/dispatcher/default.go`

- [ ] **Step 1: Add failing dispatcher initialization and fail-closed tests**

Append these tests to `linkmanager_test.go`:

```go
func TestDefaultDispatcherInitCreatesLinkRegistry(t *testing.T) {
	d := new(DefaultDispatcher)
	if err := d.Init(&Config{}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if d.LinkRegistry == nil {
		t.Fatal("dispatcher registry was not initialized")
	}
}

func TestDefaultDispatcherRegisterUserLinkFailsClosed(t *testing.T) {
	d := &DefaultDispatcher{LinkRegistry: NewLinkRegistry()}
	writer := &lifecycleWriter{}
	reader := &lifecycleReader{}
	if _, err := d.registerUserLink("node|inactive", writer, reader, "192.0.2.7"); err == nil {
		t.Fatal("inactive dispatcher registration succeeded")
	}
	if !writer.closed.Load() || !reader.interrupted.Load() {
		t.Fatal("dispatcher rejection leaked its link")
	}
}
```

- [ ] **Step 2: Run the two tests and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -run '^TestDefaultDispatcher' -count=1
```

Expected: compilation fails because `DefaultDispatcher.LinkRegistry` and `registerUserLink` do not exist.

- [ ] **Step 3: Initialize the registry and add one dispatcher registration helper**

Change the struct and `Init` in `default.go`:

```go
type DefaultDispatcher struct {
	ohm          outbound.Manager
	router       routing.Router
	policy       policy.Manager
	stats        stats.Manager
	fdns         dns.FakeDNSEngine
	Counter      sync.Map
	LinkRegistry *LinkRegistry
}

func (d *DefaultDispatcher) Init(config *Config, om outbound.Manager, router routing.Router, pm policy.Manager, sm stats.Manager) error {
	d.ohm = om
	d.router = router
	d.policy = pm
	d.stats = sm
	d.LinkRegistry = NewLinkRegistry()
	return nil
}

func (d *DefaultDispatcher) registerUserLink(user string, writer buf.Writer, reader buf.Reader, source string) (*ManagedWriter, error) {
	managed, ok := d.LinkRegistry.RegisterLink(user, writer, reader, source)
	if !ok {
		return nil, errors.New("user connection lifecycle is inactive")
	}
	return managed, nil
}
```

- [ ] **Step 4: Replace the first `LinkManagers.Load/Store/AddLink` block**

In `getLink`, replace the manager creation and `AddLink` block with:

```go
		managedWriter, registerErr := d.registerUserLink(
			user.Email,
			uplinkWriter,
			outboundLink.Reader,
			sourceIP,
		)
		if registerErr != nil {
			_ = common.Close(outboundLink.Writer)
			_ = common.Close(inboundLink.Writer)
			_ = common.Interrupt(outboundLink.Reader)
			_ = common.Interrupt(inboundLink.Reader)
			return nil, nil, nil, registerErr
		}
		inboundLink.Writer = managedWriter
```

The raw `outboundLink.Reader` is deliberately registered before later wrappers so deactivation interrupts the underlying pipe rather than a non-interruptible accounting wrapper.

- [ ] **Step 5: Replace the second `LinkManagers.Load/Store/AddLink` block**

In `DispatchLink`, register the raw reader before installing `CounterReader`:

```go
		managedWriter, registerErr := d.registerUserLink(
			user.Email,
			outbound.Writer,
			outbound.Reader,
			sourceIP,
		)
		if registerErr != nil {
			return registerErr
		}
		outbound.Writer = managedWriter
		if w != nil {
			sessionInbound.CanSpliceCopy = 3
			outbound.Writer = rate.NewRateLimitWriter(outbound.Writer, w)
		}
		var t *counter.TrafficCounter
		if c, ok := d.Counter.Load(sessionInbound.Tag); !ok {
			t = counter.NewTrafficCounter()
			d.Counter.Store(sessionInbound.Tag, t)
		} else {
			t = c.(*counter.TrafficCounter)
		}
		ts := t.GetCounter(user.Email)
		downcounter := &counter.XrayTrafficCounter{V: &ts.DownCounter}
		outbound.Reader = &CounterReader{
			Reader:  &buf.TimeoutWrapperReader{Reader: outbound.Reader},
			Counter: &ts.UpCounter,
		}
		outbound.Writer = &dispatcher.SizeStatWriter{
			Counter: downcounter,
			Writer:  outbound.Writer,
		}
```

- [ ] **Step 6: Prove both old auto-create paths are gone and run tests**

Run:

```powershell
gofmt -w core/app/dispatcher/default.go core/app/dispatcher/linkmanager_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher -count=1
$hits = rg -n 'LinkManagers|newLinkManager\(' core/app/dispatcher/default.go
if ($LASTEXITCODE -eq 0) { $hits; throw 'dispatcher still auto-creates user link managers' }
if ($LASTEXITCODE -ne 1) { throw 'registry source scan failed' }
```

Expected: dispatcher tests pass and the source scan finds no old `LinkManagers` or dispatcher-side manager construction.

- [ ] **Step 7: Commit dispatcher integration**

```powershell
git add core/app/dispatcher/default.go core/app/dispatcher/linkmanager_test.go
git commit -m "修复：收口转发连接登记入口" -m "两条 dispatcher 路径只向已授权 registry 槽位登记，失效或未初始化时立即关闭读写链路并拒绝转发。"
```

### Task 3: Bind authorization and deletion to registry lifecycle

**Files:**
- Modify: `core/user_test.go`
- Modify: `core/user.go`

- [ ] **Step 1: Add a fake user manager and failing core lifecycle tests**

Replace the `core/user_test.go` import block with this exact set, then add the helpers and tests below:

```go
import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/core/app/dispatcher"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/protocol"
)
```

```go
type fakeUserManager struct {
	mu          sync.Mutex
	users       map[string]*protocol.MemoryUser
	addCalls    int
	failAddAt   int
	removeError error
}

func newFakeUserManager() *fakeUserManager {
	return &fakeUserManager{users: make(map[string]*protocol.MemoryUser)}
}

func (m *fakeUserManager) AddUser(_ context.Context, user *protocol.MemoryUser) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCalls++
	if m.failAddAt > 0 && m.addCalls == m.failAddAt {
		return errors.New("injected add failure")
	}
	m.users[user.Email] = user
	return nil
}

func (m *fakeUserManager) RemoveUser(_ context.Context, email string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.removeError != nil {
		return m.removeError
	}
	delete(m.users, email)
	return nil
}

func (m *fakeUserManager) GetUser(_ context.Context, email string) *protocol.MemoryUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users[email]
}

func (m *fakeUserManager) GetUsers(context.Context) []*protocol.MemoryUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	users := make([]*protocol.MemoryUser, 0, len(m.users))
	for _, user := range m.users {
		users = append(users, user)
	}
	return users
}

func (m *fakeUserManager) GetUsersCount(context.Context) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.users))
}

type coreLifecycleWriter struct{ closed atomic.Bool }

func (w *coreLifecycleWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	buf.ReleaseMulti(mb)
	return nil
}

func (w *coreLifecycleWriter) Close() error {
	w.closed.Store(true)
	return nil
}

type coreLifecycleReader struct{ interrupted atomic.Bool }

func (r *coreLifecycleReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return nil, io.EOF
}

func (r *coreLifecycleReader) Interrupt() { r.interrupted.Store(true) }

func newLifecycleCore() *V2Core {
	v := New(nil)
	v.dispatcher = &dispatcher.DefaultDispatcher{LinkRegistry: dispatcher.NewLinkRegistry()}
	return v
}

func registryAccepts(v *V2Core, user string) bool {
	writer := &coreLifecycleWriter{}
	reader := &coreLifecycleReader{}
	managed, ok := v.dispatcher.LinkRegistry.RegisterLink(user, writer, reader, "192.0.2.8")
	if ok {
		_ = managed.Close()
	}
	return ok
}

func TestAddManagedUsersKeepsRegistryClosedAfterPartialFailure(t *testing.T) {
	const tag = "node:1"
	infos := []panel.UserInfo{
		{Id: 1, Uuid: "00000000-0000-0000-0000-000000000001"},
		{Id: 2, Uuid: "00000000-0000-0000-0000-000000000002"},
	}
	v := newLifecycleCore()
	manager := newFakeUserManager()
	manager.failAddAt = 2
	_, err := v.addManagedUsers(manager, tag, infos, buildVlessUsers(tag, infos, ""))
	if err == nil {
		t.Fatal("partial add failure was accepted")
	}
	if manager.GetUsersCount(context.Background()) != 0 {
		t.Fatal("newly installed credentials were not rolled back")
	}
	for _, info := range infos {
		if registryAccepts(v, format.UserTag(tag, info.Uuid)) {
			t.Fatal("failed batch activated a registry slot")
		}
	}
	if len(v.users.uidMap) != 0 {
		t.Fatal("failed batch committed UID mappings")
	}
}

func TestAddManagedUsersReusesExistingCredentialAndActivatesRegistry(t *testing.T) {
	const tag = "node:2"
	infos := []panel.UserInfo{{Id: 3, Uuid: "00000000-0000-0000-0000-000000000003"}}
	users := buildVlessUsers(tag, infos, "")
	memoryUser, err := users[0].ToMemoryUser()
	if err != nil {
		t.Fatal(err)
	}
	v := newLifecycleCore()
	manager := newFakeUserManager()
	manager.users[memoryUser.Email] = memoryUser
	if added, err := v.addManagedUsers(manager, tag, infos, users); err != nil || added != 1 {
		t.Fatalf("added=%d err=%v", added, err)
	}
	if manager.addCalls != 0 {
		t.Fatalf("existing credential was added again: calls=%d", manager.addCalls)
	}
	if !registryAccepts(v, memoryUser.Email) {
		t.Fatal("existing credential did not activate a new lifecycle")
	}
}

func TestDeactivateUsersStaysClosedWhenCredentialRemovalFails(t *testing.T) {
	const tag = "node:3"
	info := panel.UserInfo{Id: 4, Uuid: "00000000-0000-0000-0000-000000000004"}
	user := format.UserTag(tag, info.Uuid)
	v := newLifecycleCore()
	v.users.uidMap[user] = info.Id
	v.dispatcher.LinkRegistry.ActivateUser(user)
	writer := &coreLifecycleWriter{}
	reader := &coreLifecycleReader{}
	if _, ok := v.dispatcher.LinkRegistry.RegisterLink(user, writer, reader, "192.0.2.9"); !ok {
		t.Fatal("setup registration failed")
	}
	manager := newFakeUserManager()
	memoryUser, err := buildVlessUser(tag, &info, "").ToMemoryUser()
	if err != nil {
		t.Fatal(err)
	}
	manager.users[user] = memoryUser
	manager.removeError = errors.New("injected remove failure")

	v.deactivateUsers([]panel.UserInfo{info}, tag)
	v.removeManagedUsers(manager, []panel.UserInfo{info}, tag)

	if !writer.closed.Load() || !reader.interrupted.Load() {
		t.Fatal("existing connection survived deactivation")
	}
	if registryAccepts(v, user) {
		t.Fatal("credential cleanup failure reopened the data plane")
	}
	if _, exists := v.users.uidMap[user]; exists {
		t.Fatal("invalid user UID mapping remained committed")
	}
	if manager.GetUser(context.Background(), user) == nil {
		t.Fatal("test did not retain the injected stale credential")
	}
}
```

- [ ] **Step 2: Run the core tests and verify RED**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core -run '^Test(AddManagedUsers|DeactivateUsers)' -count=1
```

Expected: compilation fails because `addManagedUsers`, `deactivateUsers`, and `removeManagedUsers` do not exist.

- [ ] **Step 3: Add registry activation and fail-closed cleanup helpers**

Remove the now-unused local dispatcher import, add `log "github.com/sirupsen/logrus"`, and add these helpers to `core/user.go`:

```go
func userLinkKeys(tag string, users []panel.UserInfo) []string {
	keys := make([]string, 0, len(users))
	for i := range users {
		keys = append(keys, format.UserTag(tag, users[i].Uuid))
	}
	return keys
}

func (v *V2Core) activateUserLinks(users []string) {
	if v.dispatcher == nil || v.dispatcher.LinkRegistry == nil {
		return
	}
	for _, user := range users {
		v.dispatcher.LinkRegistry.ActivateUser(user)
	}
}

func (v *V2Core) deactivateUsers(users []panel.UserInfo, tag string) {
	for i := range users {
		closed := 0
		if v.dispatcher != nil && v.dispatcher.LinkRegistry != nil {
			closed = v.dispatcher.LinkRegistry.DeactivateUser(format.UserTag(tag, users[i].Uuid))
		}
		log.WithFields(log.Fields{
			"tag": tag, "uid": users[i].Id, "closed": closed, "reason": "user_invalidated",
		}).Info("User data plane deactivated")
	}

	v.users.mapLock.Lock()
	for i := range users {
		delete(v.users.uidMap, format.UserTag(tag, users[i].Uuid))
	}
	v.users.mapLock.Unlock()
	if v.dispatcher == nil {
		return
	}
	if value, ok := v.dispatcher.Counter.Load(tag); ok {
		traffic := value.(*counter.TrafficCounter)
		for i := range users {
			traffic.Delete(format.UserTag(tag, users[i].Uuid))
		}
	}
}

func (v *V2Core) removeManagedUsers(manager proxy.UserManager, users []panel.UserInfo, tag string) {
	for i := range users {
		user := format.UserTag(tag, users[i].Uuid)
		lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		installed := manager.GetUser(lookupCtx, user)
		lookupCancel()
		if installed == nil {
			continue
		}
		removeCtx, removeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := manager.RemoveUser(removeCtx, user)
		removeCancel()
		if err != nil {
			log.WithFields(log.Fields{
				"tag": tag, "uid": users[i].Id, "error_type": fmt.Sprintf("%T", err),
			}).Warn("Credential cleanup failed; user data plane remains closed")
		}
	}
}
```

Use `log "github.com/sirupsen/logrus"`; do not log the formatted user tag, UUID, source IP, token, or target.

- [ ] **Step 4: Replace `DelUsers` and `CloseUserIP`**

Replace the two functions with:

```go
func (vc *V2Core) DelUsers(users []panel.UserInfo, tag string, _ *panel.NodeInfo) error {
	vc.deactivateUsers(users, tag)

	if server, ok := vc.eclipse[tag]; ok {
		server.DelUsers(users)
		return nil
	}
	if server, ok := vc.mieru[tag]; ok {
		if err := server.DelUsers(users); err != nil {
			log.WithFields(log.Fields{
				"tag": tag, "error_type": fmt.Sprintf("%T", err),
			}).Warn("Mieru credential cleanup failed; user data plane remains closed")
		}
		return nil
	}

	manager, err := vc.GetUserManager(tag)
	if err != nil {
		log.WithFields(log.Fields{
			"tag": tag, "error_type": fmt.Sprintf("%T", err),
		}).Warn("User manager unavailable; user data plane remains closed")
		return nil
	}
	vc.removeManagedUsers(manager, users, tag)
	return nil
}

func (vc *V2Core) CloseUserIP(tag string, uuid string, ip string) int {
	if vc.dispatcher == nil || vc.dispatcher.LinkRegistry == nil {
		return 0
	}
	return vc.dispatcher.LinkRegistry.CloseUserIP(
		format.UserTag(tag, uuid),
		strings.TrimPrefix(strings.TrimSpace(ip), "::ffff:"),
	)
}
```

This ordering closes every registry slot before `GetUserManager`, `RemoveUser`, or Mieru restart work can block. Returning success after credential-cleanup warnings lets `Controller.applyUserList` remove limiter state and lets `commitUserStateWith` persist the invalid snapshot.

- [ ] **Step 5: Make standard user installation idempotent and activate only after complete success**

Add this helper:

```go
func (v *V2Core) addManagedUsers(manager proxy.UserManager, tag string, infos []panel.UserInfo, users []*protocol.User) (int, error) {
	readyEmails := make([]string, 0, len(users))
	newEmails := make([]string, 0, len(users))
	for _, user := range users {
		memoryUser, err := user.ToMemoryUser()
		if err != nil {
			rollbackAddedUsers(manager, newEmails)
			return 0, err
		}
		lookupCtx, lookupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		installed := manager.GetUser(lookupCtx, user.Email)
		lookupCancel()
		if installed == nil {
			addCtx, addCancel := context.WithTimeout(context.Background(), 30*time.Second)
			err = manager.AddUser(addCtx, memoryUser)
			addCancel()
			if err != nil {
				rollbackAddedUsers(manager, newEmails)
				return 0, err
			}
			newEmails = append(newEmails, user.Email)
		}
		readyEmails = append(readyEmails, user.Email)
	}
	v.commitUserUIDs(tag, infos)
	v.activateUserLinks(readyEmails)
	return len(users), nil
}
```

Replace `AddUsers` with this version, preserving the existing protocol builders:

```go
func (v *V2Core) AddUsers(p *AddUsersParams) (added int, err error) {
	if server, ok := v.eclipse[p.Tag]; ok {
		server.AddUsers(p.Users)
		v.commitUserUIDs(p.Tag, p.Users)
		v.activateUserLinks(userLinkKeys(p.Tag, p.Users))
		return len(p.Users), nil
	}
	if server, ok := v.mieru[p.Tag]; ok {
		if err := server.AddUsers(p.Users); err != nil {
			return 0, err
		}
		v.commitUserUIDs(p.Tag, p.Users)
		v.activateUserLinks(userLinkKeys(p.Tag, p.Users))
		return len(p.Users), nil
	}

	var users []*protocol.User
	switch p.NodeInfo.Type {
	case "vmess":
		users = buildVmessUsers(p.Tag, p.Users)
	case "vless":
		users = buildVlessUsers(p.Tag, p.Users, p.Common.Flow)
	case "trojan":
		users = buildTrojanUsers(p.Tag, p.Users)
	case "shadowsocks":
		users = buildSSUsers(p.Tag, p.Users, p.Common.Cipher, p.Common.ServerKey)
	case "hysteria2":
		users = buildHysteria2Users(p.Tag, p.Users)
	case "tuic":
		users = buildTuicUsers(p.Tag, p.Users)
	case "anytls":
		users = buildAnyTLSUsers(p.Tag, p.Users)
	default:
		return 0, fmt.Errorf("unsupported node type: %s", p.NodeInfo.Type)
	}
	manager, err := v.GetUserManager(p.Tag)
	if err != nil {
		return 0, fmt.Errorf("get user manager error: %s", err)
	}
	return v.addManagedUsers(manager, p.Tag, p.Users, users)
}
```

- [ ] **Step 6: Format and run the core lifecycle tests**

Run:

```powershell
gofmt -w core/user.go core/user_test.go
$env:GOEXPERIMENT='jsonv2'
go test ./core -run '^Test(AddUsersDoesNotCommitUIDMapWhenBuildFails|AddManagedUsers|DeactivateUsers)' -count=1
go test ./core/app/dispatcher ./core -count=1
```

Expected: the new lifecycle tests and existing core/dispatcher tests pass. The injected `RemoveUser` error may produce one sanitized warning but must not fail the test or reactivate the registry.

- [ ] **Step 7: Commit the authorization lifecycle integration**

```powershell
git add core/user.go core/user_test.go
git commit -m "修复：用户失效立即关闭转发连接" -m "新增用户只在认证完整成功后激活连接槽；删除用户先关闭数据面，再容错清理认证、计数和本地状态。"
```

### Task 4: Verify the full affected chain and finalize the learning record

**Files:**
- Modify: `LESSONS_LEARNED.md`

- [ ] **Step 1: Run targeted lifecycle and existing synchronization regressions**

Run:

```powershell
$env:GOEXPERIMENT='jsonv2'
go test ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit -count=1
go test ./node -run '^Test(RemoveExpiredUsers|NextUserExpiry|LocalExpiry|ApplyUserList|CommitUserState)' -count=1
```

Expected: all listed packages and selected node-side expiry/snapshot tests pass with zero failures.

- [ ] **Step 2: Run static checks and source-boundary scans**

Run:

```powershell
go vet ./core/app/dispatcher ./core ./limiter ./node ./common/accessaudit
$hits = rg -n 'LinkManagers|\.Store\(user\.Email, lm\)|\.AddLink\(managedWriter' core --glob '*.go'
if ($LASTEXITCODE -eq 0) { $hits; throw 'legacy connection registration remains' }
if ($LASTEXITCODE -ne 1) { throw 'legacy registration scan failed' }
git diff --check
```

Expected: vet exits zero, the source scan finds no legacy auto-create path, and Git reports no whitespace errors.

- [ ] **Step 3: Run the race suite on a Linux build host**

Run on Linux with CGO enabled:

```bash
GOEXPERIMENT=jsonv2 go test -race ./core/app/dispatcher ./core ./node -count=1
```

Expected: exit zero with no race report. The current Windows workstation has no usable race toolchain; if Linux execution is unavailable, record that exact verification gap instead of claiming race coverage.

- [ ] **Step 4: Update the 2026-08-17 lesson with implemented behavior**

In the existing lesson, change the diagnosis-only wording to record these concrete facts:

```markdown
### 已实施修复与验证

- dispatcher 已改为权威 `LinkRegistry`：只有 `AddUsers` 能激活用户槽位，两条代理登记路径只能加入已激活槽位；`DeactivateUser` 原子删除槽位并在锁外关闭 writer、interrupt 原始 reader。
- `DelUsers` 对整批用户先关闭数据面，再执行 Xray、自定义入站和 Mieru 的认证清理。认证清理失败只产生脱敏告警，不会重新开放 registry，也不会阻止 limiter 更新和离线用户快照提交。
- 标准协议重复添加时先通过 `UserManager.GetUser` 识别相同已安装凭据；批量新增失败只回滚本次新装凭据，任何失败批次都不会提前激活连接槽。
- 回归覆盖未激活拒绝、停用后拒绝复活、登记/停用锁边界、重新授权 generation 隔离、批量新增回滚和认证清理失败时保持 fail-closed。FlowTraffic 继续使用非阻塞 `TrySubmit`，不进入连接关闭等待路径。
- 本地验证记录实际执行的 `go test`、`go vet` 和 `git diff --check` 结果。Linux race、生产二进制版本核对和转发机 TCP/RSS/FD 灰度结果必须分别记录；没有执行的项目明确标为未验证。
```

Keep the existing production discrimination guidance for `ESTABLISHED`, `TIME_WAIT`, `CLOSE_WAIT`, conntrack, and old binaries. Do not record a host name, full IP, UUID, token, destination, or customer identity.

- [ ] **Step 5: Review scope and commit the lesson**

Run:

```powershell
git status --short
git diff -- core/app/dispatcher core/user.go core/user_test.go LESSONS_LEARNED.md
git diff --check
```

Expected: only the planned implementation/test files and `LESSONS_LEARNED.md` are present; no unrelated user changes are staged.

Commit only the lesson after inserting the actual verification results:

```powershell
git add LESSONS_LEARNED.md
git commit -m "文档：记录用户连接立即回收修复" -m "补充权威连接注册表、失效清理顺序、并发回归和生产灰度边界，便于后续排查 TCP 与内存不回落。"
```

- [ ] **Step 6: Report implementation and deployment as separate states**

Report the code and test commit hashes, exact commands that passed, and any Linux race gap. State explicitly that production is unchanged until a new v2node binary is built and rolled out. The first gray node must verify that the test user's forwarding-machine `ESTABLISHED` connections reach zero after the node applies invalidation, that new requests remain rejected, and that reauthorization creates only new connections; then observe TCP states, RSS, goroutines, FDs, FlowTraffic pending/gap, panic, OOM, and restarts for at least 15 minutes.
