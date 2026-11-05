package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/conf"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/inbound"
)

type startupUserLoader interface {
	AddNodeWithUsers(string, *panel.NodeInfo, []panel.UserInfo) (int, error)
}

type startupInspectingManager struct {
	inbound.Manager
	inspect func(inbound.Handler) error
}

func (m *startupInspectingManager) AddHandler(ctx context.Context, h inbound.Handler) error {
	if err := m.inspect(h); err != nil {
		return err
	}
	return m.Manager.AddHandler(ctx, h)
}

func TestStartupLoadsUsersBeforePublishingInbound(t *testing.T) {
	v := New(conf.New())
	loader, ok := any(v).(startupUserLoader)
	if !ok {
		t.Fatal("startup cannot load users before publishing its listener")
	}
	if err := v.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	info := routeCompileNode(9001)
	info.Type = "shadowsocks"
	info.Common.Protocol = "shadowsocks"
	info.Common.Cipher = "aes-128-gcm"
	info.Common.ListenIP = "127.0.0.1"
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	info.Common.ServerPort = listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	users := make([]panel.UserInfo, 2048)
	for i := range users {
		users[i] = panel.UserInfo{Id: i + 1, Uuid: fmt.Sprintf("startup-test-%08d", i)}
	}
	inspected := false
	v.ihm = &startupInspectingManager{Manager: v.ihm, inspect: func(h inbound.Handler) error {
		inspected = true
		manager, err := inboundUserManager(h, info.Tag)
		if err != nil {
			return err
		}
		for _, user := range users {
			if manager.GetUser(context.Background(), format.UserTag(info.Tag, user.Uuid)) == nil {
				return errors.New("listener published before all credentials were loaded")
			}
		}
		return nil
	}}
	added, err := loader.AddNodeWithUsers(info.Tag, info, users)
	if err != nil || added != len(users) || !inspected {
		t.Fatalf("added=%d inspected=%v err=%v", added, inspected, err)
	}
	if _, err := loader.AddNodeWithUsers(info.Tag, info, users); err == nil {
		t.Fatal("duplicate startup replaced an existing inbound")
	}
	manager, err := v.GetUserManager(info.Tag)
	if err != nil || manager.GetUser(context.Background(), format.UserTag(info.Tag, users[0].Uuid)) == nil {
		t.Fatal("duplicate startup damaged the active user's credentials")
	}
}

func TestStartupBindFailureRemovesPreparedUserState(t *testing.T) {
	v := New(conf.New())
	loader, ok := any(v).(startupUserLoader)
	if !ok {
		t.Fatal("startup cannot atomically activate a prepared inbound")
	}
	if err := v.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info := routeCompileNode(9002)
	info.Type = "shadowsocks"
	info.Common.Protocol = "shadowsocks"
	info.Common.Cipher = "aes-128-gcm"
	info.Common.ListenIP = "127.0.0.1"
	info.Common.ServerPort = listener.Addr().(*net.TCPAddr).Port
	users := []panel.UserInfo{{Id: 1, Uuid: "startup-bind-test"}}
	if _, err := loader.AddNodeWithUsers(info.Tag, info, users); err == nil {
		t.Fatal("expected occupied port to reject activation")
	}
	if _, err := v.GetUserManager(info.Tag); err == nil {
		t.Fatal("failed inbound remained registered")
	}
	v.users.mapLock.RLock()
	defer v.users.mapLock.RUnlock()
	if len(v.users.uidMap) != 0 {
		t.Fatal("failed startup retained prepared user IDs")
	}
}

type reloadTestHandler struct{ tag string }

func (h *reloadTestHandler) Start() error                           { return nil }
func (h *reloadTestHandler) Close() error                           { return nil }
func (h *reloadTestHandler) Tag() string                            { return h.tag }
func (h *reloadTestHandler) ReceiverSettings() *serial.TypedMessage { return nil }
func (h *reloadTestHandler) ProxySettings() *serial.TypedMessage    { return nil }

func TestSwapPreparedInboundSuccess(t *testing.T) {
	candidate := &reloadTestHandler{tag: "node-302"}
	rollback := &reloadTestHandler{tag: "node-302"}
	var calls []string
	err := swapPreparedInbound("node-302", candidate, rollback, inboundSwapHooks{
		remove: func(tag string) error {
			calls = append(calls, "remove:"+tag)
			return nil
		},
		add: func(handler inbound.Handler) error {
			calls = append(calls, "add:candidate")
			return nil
		},
		close: func(handler inbound.Handler) error {
			calls = append(calls, "close:rollback")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"remove:node-302", "add:candidate", "close:rollback"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}

func TestSwapPreparedInboundRestoresRollbackAfterCandidateFailure(t *testing.T) {
	candidate := &reloadTestHandler{tag: "node-302"}
	rollback := &reloadTestHandler{tag: "node-302"}
	var calls []string
	addCalls := 0
	err := swapPreparedInbound("node-302", candidate, rollback, inboundSwapHooks{
		remove: func(tag string) error {
			if addCalls == 0 {
				calls = append(calls, "remove:old")
			} else {
				calls = append(calls, "remove:failed-candidate")
			}
			return nil
		},
		add: func(handler inbound.Handler) error {
			addCalls++
			if addCalls == 1 {
				calls = append(calls, "add:candidate")
				return errors.New("candidate failed")
			}
			calls = append(calls, "add:rollback")
			return nil
		},
		close: func(handler inbound.Handler) error {
			calls = append(calls, "close:orphan")
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected candidate activation error")
	}
	want := []string{"remove:old", "add:candidate", "remove:failed-candidate", "add:rollback"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v want=%v", calls, want)
	}
}
