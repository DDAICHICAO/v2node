package core

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/inbound"
)

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
