package core

import (
	"encoding/binary"
	"io"
	"net"
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

func TestMieruAcceptProxyProtocolSetting(t *testing.T) {
	info := &panel.NodeInfo{
		Common: &panel.CommonNode{
			NetworkSettings: []byte(`{"acceptProxyProtocol":true,"transports":["TCP","UDP"]}`),
		},
	}

	got, err := mieruAcceptProxyProtocol(info)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("acceptProxyProtocol was not enabled")
	}
}

func TestPrepareMieruProxyProtocolConnV1(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		_, _ = server.Write([]byte("PROXY TCP4 203.0.113.9 198.51.100.1 12345 443\r\nhello"))
	}()

	conn, err := prepareMieruProxyProtocolConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteIP(conn.RemoteAddr()); got != "203.0.113.9" {
		t.Fatalf("remote addr = %q", got)
	}
	payload, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestPrepareMieruProxyProtocolConnV2TCP4(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		header := make([]byte, 16+12)
		copy(header[:12], []byte("\r\n\r\n\x00\r\nQUIT\n"))
		header[12] = 0x21 // v2, PROXY command.
		header[13] = 0x11 // TCP over IPv4.
		binary.BigEndian.PutUint16(header[14:16], 12)
		copy(header[16:20], net.ParseIP("203.0.113.10").To4())
		copy(header[20:24], net.ParseIP("198.51.100.1").To4())
		binary.BigEndian.PutUint16(header[24:26], 23456)
		binary.BigEndian.PutUint16(header[26:28], 443)
		_, _ = server.Write(append(header, []byte("world")...))
	}()

	conn, err := prepareMieruProxyProtocolConn(client)
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteIP(conn.RemoteAddr()); got != "203.0.113.10" {
		t.Fatalf("remote addr = %q", got)
	}
	payload, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "world" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestParseMieruProxyProtocolDatagramV2UDP4(t *testing.T) {
	payload := []byte("ciphertext")
	packet := make([]byte, 16+12+len(payload))
	copy(packet[:12], []byte("\r\n\r\n\x00\r\nQUIT\n"))
	packet[12] = 0x21 // v2, PROXY command.
	packet[13] = 0x12 // UDP over IPv4.
	binary.BigEndian.PutUint16(packet[14:16], 12)
	copy(packet[16:20], net.ParseIP("203.0.113.9").To4())
	copy(packet[20:24], net.ParseIP("198.51.100.1").To4())
	binary.BigEndian.PutUint16(packet[24:26], 12345)
	binary.BigEndian.PutUint16(packet[26:28], 443)
	copy(packet[28:], payload)

	relay := &net.UDPAddr{IP: net.ParseIP("10.0.0.1"), Port: 13003}
	addr, offset, ok, err := parseMieruProxyProtocolDatagram(packet, relay)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("proxy protocol datagram was not detected")
	}
	if offset != 28 {
		t.Fatalf("offset = %d", offset)
	}
	if got := remoteIP(addr); got != "203.0.113.9" {
		t.Fatalf("remote addr = %q", got)
	}
	relayAddr, ok := addr.(interface{ proxyProtocolRelayAddr() net.Addr })
	if !ok {
		t.Fatal("missing relay address")
	}
	if got := relayAddr.proxyProtocolRelayAddr().String(); got != relay.String() {
		t.Fatalf("relay addr = %q", got)
	}
	if got := string(packet[offset:]); got != string(payload) {
		t.Fatalf("payload = %q", got)
	}
}

func TestPrepareMieruProxyProtocolConnWithoutHeader(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	go func() {
		defer server.Close()
		_, _ = server.Write([]byte("hello"))
	}()

	conn, err := prepareMieruProxyProtocolConn(client)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "hello" {
		t.Fatalf("payload = %q", payload)
	}
}
