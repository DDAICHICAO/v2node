package core

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy/socks"
	"github.com/xtls/xray-core/transport"
)

// managedSocksServer keeps credentials mutable without replacing a live listener.
// The core SOCKS implementation still owns CONNECT, UDP ASSOCIATE and forwarding.
type managedSocksServer struct {
	tag         string
	address     string
	udp         bool
	xray        *xcore.Instance
	dispatcher  routing.Dispatcher
	mu          sync.Mutex
	users       map[string]int
	connections map[net.Conn]string
	listener    net.Listener
	closed      bool
	handshakes  chan struct{}
}

func newManagedSocksServer(tag string, info *panel.NodeInfo, xray *xcore.Instance, dispatcher routing.Dispatcher) (*managedSocksServer, error) {
	if info == nil || info.Common == nil || info.Security != 0 || info.Common.Tls != 0 || (info.Common.Network != "" && info.Common.Network != "tcp") {
		return nil, errors.New("SOCKS5 requires raw TCP without TLS")
	}
	var settings struct {
		UDP                 bool `json:"udp"`
		AcceptProxyProtocol bool `json:"acceptProxyProtocol"`
	}
	if len(info.Common.NetworkSettings) > 0 && string(info.Common.NetworkSettings) != "null" {
		if err := json.Unmarshal(info.Common.NetworkSettings, &settings); err != nil {
			return nil, errors.New("invalid SOCKS5 network settings")
		}
	}
	if settings.AcceptProxyProtocol {
		return nil, errors.New("SOCKS5 PROXY protocol is unsupported")
	}
	if info.Common.ServerPort < 1 || info.Common.ServerPort > 65535 {
		return nil, errors.New("invalid SOCKS5 port")
	}
	ip := info.Common.ListenIP
	if ip == "" {
		ip = "0.0.0.0"
	}
	if net.ParseIP(ip) == nil {
		return nil, errors.New("invalid SOCKS5 listen IP")
	}
	return &managedSocksServer{tag: tag, address: net.JoinHostPort(ip, strconv.Itoa(info.Common.ServerPort)), udp: settings.UDP, xray: xray, dispatcher: dispatcher, users: make(map[string]int), connections: make(map[net.Conn]string), handshakes: make(chan struct{}, 256)}, nil
}

func validateSocksUsers(users []panel.UserInfo) error {
	for _, u := range users {
		if u.Id < 1 || len(u.Uuid) < 1 || len(u.Uuid) > 255 {
			return errors.New("invalid SOCKS5 user credential")
		}
	}
	return nil
}

func (s *managedSocksServer) AddUsers(users []panel.UserInfo) error {
	if err := validateSocksUsers(users); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, u := range users {
		s.users[u.Uuid] = u.Id
	}
	return nil
}

func (s *managedSocksServer) DelUsers(users []panel.UserInfo) {
	s.mu.Lock()
	revoked := make(map[string]bool, len(users))
	for _, u := range users {
		delete(s.users, u.Uuid)
		revoked[u.Uuid] = true
	}
	var conns []net.Conn
	for conn, uuid := range s.connections {
		if revoked[uuid] {
			conns = append(conns, conn)
		}
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *managedSocksServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("SOCKS5 listener is closed")
	}
	ln, err := net.Listen("tcp", s.address)
	if err != nil {
		return err
	}
	s.listener = ln
	go s.accept(ln)
	return nil
}

func (s *managedSocksServer) accept(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case s.handshakes <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			<-s.handshakes
			_ = conn.Close()
			return
		}
		s.connections[conn] = ""
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *managedSocksServer) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.listener
	conns := make([]net.Conn, 0, len(s.connections))
	for c := range s.connections {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

func (s *managedSocksServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock() }()
	held := true
	defer func() {
		if held {
			<-s.handshakes
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 || head[1] == 0 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if !bytes.Contains(methods, []byte{2}) {
		_, _ = conn.Write([]byte{5, 255})
		return
	}
	if _, err := conn.Write([]byte{5, 2}); err != nil {
		return
	}
	authHead := make([]byte, 2)
	if _, err := io.ReadFull(conn, authHead); err != nil || authHead[0] != 1 || authHead[1] == 0 {
		return
	}
	user := make([]byte, int(authHead[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil || plen[0] == 0 {
		return
	}
	pass := make([]byte, int(plen[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return
	}
	uuid := string(user)
	s.mu.Lock()
	_, exists := s.users[uuid]
	valid := exists && !s.closed && subtle.ConstantTimeCompare(user, pass) == 1
	if valid {
		s.connections[conn] = uuid
	}
	s.mu.Unlock()
	if !valid {
		_, _ = conn.Write([]byte{1, 1})
		return
	}
	// Replay only the already validated handshake to the core's per-connection
	// immutable account. Suppress its two duplicate negotiation/auth replies.
	replay := append(append(append(append(append([]byte{}, head...), methods...), authHead...), user...), plen...)
	replay = append(replay, pass...)
	wrapped := &socksHandshakeConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(replay), conn), skip: 4}
	if _, err := conn.Write([]byte{1, 0}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	<-s.handshakes
	held = false
	ctx := context.WithValue(context.Background(), xcore.XrayKey(1), s.xray)
	ctx = context.WithValue(ctx, "cone", false)
	gateway := xnet.DestinationFromAddr(conn.LocalAddr())
	ctx = session.ContextWithInbound(ctx, &session.Inbound{Tag: s.tag, Name: "socks", Source: xnet.DestinationFromAddr(conn.RemoteAddr()), Local: gateway, Gateway: gateway, Conn: conn, CanSpliceCopy: 3})
	ctx = session.ContextWithContent(ctx, &session.Content{SniffingRequest: session.SniffingRequest{Enabled: true, OverrideDestinationForProtocol: []string{"http", "tls", "quic"}}})
	server, err := socks.NewServer(ctx, &socks.ServerConfig{AuthType: socks.AuthType_PASSWORD, Accounts: map[string]string{uuid: uuid}, UdpEnabled: s.udp})
	if err != nil {
		return
	}
	dispatch := &socksUserDispatcher{Dispatcher: s.dispatcher, email: format.UserTag(s.tag, uuid)}
	_ = server.Process(ctx, xnet.Network_TCP, wrapped, dispatch)
}

type socksHandshakeConn struct {
	net.Conn
	reader io.Reader
	skip   int
}

func (c *socksHandshakeConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
func (c *socksHandshakeConn) Write(p []byte) (int, error) {
	original := len(p)
	skip := min(c.skip, len(p))
	c.skip -= skip
	p = p[skip:]
	if len(p) == 0 {
		return original, nil
	}
	n, err := c.Conn.Write(p)
	return n + skip, err
}

type socksUserDispatcher struct {
	routing.Dispatcher
	email string
}

func (d *socksUserDispatcher) userContext(ctx context.Context) context.Context {
	if in := session.InboundFromContext(ctx); in != nil {
		copy := *in
		copy.User = &protocol.MemoryUser{Email: d.email}
		copy.CanSpliceCopy = 3
		ctx = session.ContextWithInbound(ctx, &copy)
	}
	return ctx
}
func (d *socksUserDispatcher) Dispatch(ctx context.Context, dest xnet.Destination) (*transport.Link, error) {
	return d.Dispatcher.Dispatch(d.userContext(ctx), dest)
}
func (d *socksUserDispatcher) DispatchLink(ctx context.Context, dest xnet.Destination, link *transport.Link) error {
	return d.Dispatcher.DispatchLink(d.userContext(ctx), dest, link)
}

func (v *V2Core) addSocksNode(tag string, info *panel.NodeInfo, users []panel.UserInfo) (int, error) {
	if v.socks == nil {
		v.socks = make(map[string]*managedSocksServer)
	}
	if v.socks[tag] != nil {
		return 0, errors.New("SOCKS5 inbound tag already exists")
	}
	server, err := newManagedSocksServer(tag, info, v.Server, v.dispatcher)
	if err != nil {
		return 0, err
	}
	if err = server.AddUsers(users); err != nil {
		return 0, err
	}
	v.commitUserUIDs(tag, users)
	v.activateUserLinks(userLinkKeys(tag, users))
	if err = server.Start(); err != nil {
		v.deactivateUsers(users, tag)
		return 0, fmt.Errorf("start SOCKS5 listener: %w", err)
	}
	v.socks[tag] = server
	return len(users), nil
}
