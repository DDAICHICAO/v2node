package core

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

func TestAddUsersDoesNotCommitUIDMapWhenBuildFails(t *testing.T) {
	v := New(nil)
	_, err := v.AddUsers(&AddUsersParams{
		Tag: "bad-node",
		Users: []panel.UserInfo{
			{Id: 1, Uuid: "new-user"},
		},
		NodeInfo: &panel.NodeInfo{Type: "unsupported"},
	})
	if err == nil {
		t.Fatal("expected unsupported node type to fail")
	}

	v.users.mapLock.RLock()
	defer v.users.mapLock.RUnlock()
	if len(v.users.uidMap) != 0 {
		t.Fatalf("expected uid map to stay empty after failed add, got %+v", v.users.uidMap)
	}
}

func TestSntpEclipseOnlineRefreshIntervalUsesBaseConfig(t *testing.T) {
	got := sntpEclipseOnlineRefreshInterval(&panel.NodeInfo{
		Common: &panel.CommonNode{
			BaseConfig: &panel.BaseConfig{
				SntpEclipseOnlineRefresh: "9",
			},
		},
	})
	if got != 9*time.Second {
		t.Fatalf("expected online refresh interval 9s, got %s", got)
	}

	if got := sntpEclipseOnlineRefreshInterval(nil); got != defaultSntpEclipseOnlineRefresh {
		t.Fatalf("expected default online refresh interval, got %s", got)
	}
}

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
