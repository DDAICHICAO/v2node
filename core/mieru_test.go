package core

import (
	"testing"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func TestAddMieruUserKeepsDeviceUUIDs(t *testing.T) {
	users := map[string]mieruUser{}

	addMieruUser(users, panel.UserInfo{Id: 123, Uuid: "device-a"})
	addMieruUser(users, panel.UserInfo{Id: 123, Uuid: "device-b"})

	if _, ok := users["device-a"]; !ok {
		t.Fatal("missing first device UUID username")
	}
	if _, ok := users["device-b"]; !ok {
		t.Fatal("missing second device UUID username")
	}
	if got := users["device-a"].UUID; got != "device-a" {
		t.Fatalf("device-a mapped to %q", got)
	}
	if got := users["device-b"].UUID; got != "device-b" {
		t.Fatalf("device-b mapped to %q", got)
	}
	if got := users["123"].UUID; got != "device-a" {
		t.Fatalf("legacy uid alias mapped to %q", got)
	}
}
