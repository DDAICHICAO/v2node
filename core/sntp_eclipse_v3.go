package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/sntpv3"
	"github.com/wyx2685/v2node/limiter"
)

type eclipseV3Record struct {
	conn  *sntpv3.Conn
	uuid  string
	start sync.Once
	serve func()
}
type eclipseV3Runtime struct {
	owner        *SntpEclipseServer
	server       *http.Server
	certificate  atomic.Pointer[tls.Certificate]
	identity     [32]byte
	domain, path string
	opts         sntpv3.Options
	maxSessions  int
	admissions   chan struct{}
	connections  chan struct{}
	mu           sync.Mutex
	sessions     map[[16]byte]*eclipseV3Record
	stopped      bool
	listening    atomic.Bool
	wires        map[net.Conn]struct{}
}

func v3Identity(info *panel.NodeInfo) [32]byte {
	b, _ := json.Marshal(struct {
		Listen     string
		Port       int
		TLS        int
		Network    string
		Settings   json.RawMessage
		Encryption panel.EncSettings
		Name       string
	}{info.Common.ListenIP, info.Common.ServerPort, info.Security, info.Common.Network, info.Common.NetworkSettings, info.Common.EncryptionSettings, info.Common.TlsSettings.PrimaryServerName()})
	return sha256.Sum256(b)
}
func newEclipseV3(s *SntpEclipseServer, info *panel.NodeInfo) (*eclipseV3Runtime, error) {
	common := info.Common
	if info.Security != panel.Tls || common.Network != "ws" || common.CertInfo == nil {
		return nil, errors.New("Titan V3 requires TLS and WebSocket")
	}
	domain := strings.TrimSpace(common.TlsSettings.PrimaryServerName())
	if domain == "" || net.ParseIP(domain) != nil {
		return nil, errors.New("Titan V3 requires a certificate domain")
	}
	var network struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(common.NetworkSettings, &network); err != nil {
		return nil, fmt.Errorf("V3 network settings: %w", err)
	}
	if network.Path == "" {
		network.Path = "/ws"
	}
	if !strings.HasPrefix(network.Path, "/") || strings.ContainsAny(network.Path, "?#\r\n") || len(network.Path) > 256 {
		return nil, errors.New("invalid V3 WebSocket path")
	}
	enc := common.EncryptionSettings
	window := enc.ResumeWindow
	if window == 0 {
		window = 30
	}
	buffer := enc.ResumeBufferBytes
	if buffer == 0 {
		buffer = 1 << 20
	}
	sessions := enc.MaxSessions
	if sessions == 0 {
		sessions = 256
	}
	if window < 3 || window > 120 || buffer < sntpv3.MaxPayload || buffer > 4<<20 || sessions < 1 || sessions > 4096 {
		return nil, errors.New("invalid V3 resource limits")
	}
	r := &eclipseV3Runtime{owner: s, identity: v3Identity(info), domain: domain, path: network.Path, opts: sntpv3.Options{SendBudget: sntpv3.NewBudget(64 << 20), ReceiveBudget: sntpv3.NewBudget(64 << 20), Window: time.Duration(window) * time.Second, BufferBytes: buffer}, maxSessions: sessions, admissions: make(chan struct{}, 64), connections: make(chan struct{}, sessions+64), sessions: make(map[[16]byte]*eclipseV3Record), wires: make(map[net.Conn]struct{})}
	if err := r.updateCertificate(info); err != nil {
		return nil, err
	}
	return r, nil
}
func (r *eclipseV3Runtime) updateCertificate(info *panel.NodeInfo) error {
	if info == nil || info.Common == nil || info.Common.CertInfo == nil || v3Identity(info) != r.identity {
		return errors.New("V3 hot update only permits a certificate replacement")
	}
	cert, err := tls.LoadX509KeyPair(info.Common.CertInfo.CertFile, info.Common.CertInfo.KeyFile)
	if err != nil {
		return fmt.Errorf("load V3 certificate: %w", err)
	}
	if cert.Leaf == nil {
		cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
	}
	if err := cert.Leaf.VerifyHostname(r.domain); err != nil {
		return fmt.Errorf("V3 certificate name: %w", err)
	}
	now := time.Now()
	if now.Before(cert.Leaf.NotBefore) || !now.Before(cert.Leaf.NotAfter) {
		return errors.New("V3 certificate expired or not yet valid")
	}
	r.certificate.Store(&cert)
	return nil
}

type v3SourceKey struct{}
type v3SourceConn struct {
	net.Conn
	owner     *SntpEclipseServer
	once      sync.Once
	prepared  net.Conn
	err       error
	mu        sync.Mutex
	ip        string
	closeOnce sync.Once
	release   func()
}

func (c *v3SourceConn) Read(p []byte) (int, error) {
	c.once.Do(func() {
		prepared, ip, err := c.owner.prepareConn(c.Conn)
		c.prepared, c.err = prepared, err
		c.mu.Lock()
		c.ip = ip
		c.mu.Unlock()
	})
	if c.err != nil {
		return 0, c.err
	}
	return c.prepared.Read(p)
}
func (c *v3SourceConn) Close() error {
	err := c.Conn.Close()
	c.closeOnce.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return err
}
func (c *v3SourceConn) clientIP() string { c.mu.Lock(); defer c.mu.Unlock(); return c.ip }

type v3TLSListener struct {
	net.Listener
	owner       *SntpEclipseServer
	config      *tls.Config
	connections chan struct{}
}

func (l *v3TLSListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.connections <- struct{}{}:
			source := &v3SourceConn{Conn: raw, owner: l.owner, release: func() { <-l.connections }}
			return tls.Server(source, l.config), nil
		default:
			raw.Close()
		}
	}
}

type eclipseWSConn struct {
	net.Conn
	ws *websocket.Conn
}

func (c *eclipseWSConn) Close() error { return c.ws.CloseNow() }

func (r *eclipseV3Runtime) start(ln net.Listener) error {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if !strings.EqualFold(strings.TrimSuffix(hello.ServerName, "."), strings.TrimSuffix(r.domain, ".")) {
			return nil, errors.New("unrecognized certificate name")
		}
		return r.certificate.Load(), nil
	}}
	r.server = &http.Server{Handler: http.HandlerFunc(r.handle), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			if c, ok := conn.(*tls.Conn); ok {
				if raw, ok := c.NetConn().(*v3SourceConn); ok {
					return context.WithValue(ctx, v3SourceKey{}, raw)
				}
			}
			return ctx
		}}
	r.listening.Store(true)
	go func() {
		err := r.server.Serve(&v3TLSListener{Listener: ln, owner: r.owner, config: cfg, connections: r.connections})
		r.listening.Store(false)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.WithFields(log.Fields{"tag": r.owner.tag, "err": err}).Error("SNTP Titan V3 listener stopped")
			r.close()
		}
	}()
	return nil
}
func (r *eclipseV3Runtime) handle(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet || req.URL.Path != r.path || req.URL.RawQuery != "" || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "<!doctype html><html><head><title>Service</title></head><body>Service online.</body></html>")
		return
	}
	select {
	case r.admissions <- struct{}{}:
	default:
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
		return
	}
	admitted := true
	release := func() {
		if admitted {
			<-r.admissions
			admitted = false
		}
	}
	defer release()
	ws, err := websocket.Accept(w, req, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	raw := &eclipseWSConn{Conn: websocket.NetConn(context.Background(), ws, websocket.MessageBinary), ws: ws}
	ws.SetReadLimit(64 << 10)
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		raw.Close()
		return
	}
	r.wires[raw] = struct{}{}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.wires, raw); r.mu.Unlock() }()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	var selector [1]byte
	if _, err = io.ReadFull(raw, selector[:]); err != nil {
		raw.Close()
		return
	}
	ip, _, _ := net.SplitHostPort(req.RemoteAddr)
	if src, ok := req.Context().Value(v3SourceKey{}).(*v3SourceConn); ok {
		ip = src.clientIP()
	}
	if selector[0] == 1 {
		var record *eclipseV3Record
		err = sntpv3.AcceptResume(raw, func(id [16]byte) *sntpv3.Conn {
			r.mu.Lock()
			rec := r.sessions[id]
			r.mu.Unlock()
			if rec == nil {
				return nil
			}
			if _, ok := r.owner.lookupUser(rec.uuid); !ok {
				return nil
			}
			record = rec
			return rec.conn
		}, func() error {
			// Apply source policy only after the cryptographic proof is valid.
			if record == nil {
				return errors.New("V3 session unavailable")
			}
			if _, ok := r.owner.lookupUser(record.uuid); !ok {
				return errors.New("V3 user no longer authorized")
			}
			if limit, err := limiter.GetLimiter(r.owner.tag); err == nil {
				if _, reject, _ := limit.CheckLimit(format.UserTag(r.owner.tag, record.uuid), ip, true); reject {
					return errors.New("V3 recovery source rejected")
				}
			}
			return nil
		})
		if err != nil {
			raw.Close()
			return
		}
		if record != nil {
			record.start.Do(func() { go record.serve() })
		}
		return
	}
	if selector[0] != 0 {
		raw.Close()
		return
	}
	// New session authentication is never raced; only WSS preparation is raced.
	r.owner.handleAuthenticatedConn(raw, ip, release)
}
func (r *eclipseV3Runtime) newSession(session *sntpEclipseSession, key [32]byte, release func()) error {
	raw := session.conn
	c := sntpv3.New(session.hello.SessionID, key, 1, r.opts)
	session.conn = c
	session.v3 = true
	rec := &eclipseV3Record{conn: c, uuid: session.hello.UUID, serve: func() {
		switch session.hello.Command {
		case sntpEclipseCmdTCP:
			session.serveTCP()
		case sntpEclipseCmdUDP:
			session.serveUDP()
		default:
			c.Abort()
		}
	}}
	r.mu.Lock()
	if r.stopped || len(r.sessions) >= r.maxSessions || r.sessions[c.ID()] != nil {
		r.mu.Unlock()
		c.Abort()
		return errors.New("V3 session limit exceeded")
	}
	r.sessions[c.ID()] = rec
	r.mu.Unlock()
	c.SetOnClose(func() {
		r.mu.Lock()
		if r.sessions[c.ID()] == rec {
			delete(r.sessions, c.ID())
		}
		r.mu.Unlock()
	})
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if err := sntpv3.WriteWelcome(raw, c.ID(), key, r.opts); err != nil {
		raw.Close()
		release()
		return err
	}
	if err := sntpv3.ReadReady(raw, c.ID(), key); err != nil {
		raw.Close()
		release()
		return err
	}
	// A late initial confirmation cannot replace a connection already resumed.
	if err := c.Attach(raw, 1, 0, 0); err != nil {
		raw.Close()
		release()
		return err
	}
	release()
	rec.start.Do(func() { go rec.serve() })
	return nil
}
func (r *eclipseV3Runtime) close() {
	r.listening.Store(false)
	r.mu.Lock()
	r.stopped = true
	wires := make([]net.Conn, 0, len(r.wires))
	for w := range r.wires {
		wires = append(wires, w)
	}
	r.mu.Unlock()
	for _, w := range wires {
		_ = w.Close()
	}
	if r.server != nil {
		_ = r.server.Close()
	}
	r.mu.Lock()
	all := make([]*sntpv3.Conn, 0, len(r.sessions))
	for _, rec := range r.sessions {
		all = append(all, rec.conn)
	}
	r.mu.Unlock()
	for _, c := range all {
		c.Abort()
	}
}
