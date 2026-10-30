package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/common/titannative"
	"github.com/wyx2685/v2node/limiter"
)

var titanRuntimeServers sync.Map
var nativeProcessPending atomic.Int64
var nativeProcessActive atomic.Int64

type nativeIngressBudget struct {
	pending, active                                         atomic.Int64
	rejected, invalid, unauthorized, acceptedV2, acceptedV3 atomic.Uint64
	dispatchFailed, preauthRejected, replayRejected         atomic.Uint64
	maxPending, maxActive                                   int64
	maxUser                                                 int
	preauthRate, preauthBurst                               float64
	mu                                                      sync.Mutex
	users                                                   map[int]int
	ipMu                                                    sync.Mutex
	ipBuckets                                               map[string]nativeIPBucket
	ipLastCleanup                                           time.Time
}

type nativeIPBucket struct {
	tokens  float64
	updated time.Time
}

func nativeBound(value, fallback, low, high int) int {
	if value == 0 {
		return fallback
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
func newNativeIngressBudget(e panel.EncSettings) *nativeIngressBudget {
	rate := nativeBound(e.NativePreauthRatePerIP, 128, 1, 8192)
	burst := nativeBound(e.NativePreauthBurstPerIP, 256, 1, 32768)
	if burst < rate {
		burst = rate
	}
	return &nativeIngressBudget{
		maxPending:    int64(nativeBound(e.NativeMaxHandshakes, 4096, 64, 32768)),
		maxActive:     int64(nativeBound(e.NativeMaxConnections, 131072, 1024, 262144)),
		maxUser:       nativeBound(e.NativeMaxUserConnections, 4096, 64, 32768),
		preauthRate:   float64(rate),
		preauthBurst:  float64(burst),
		users:         make(map[int]int),
		ipBuckets:     make(map[string]nativeIPBucket),
		ipLastCleanup: time.Now(),
	}
}
func reserve(a *atomic.Int64, max int64) bool {
	for {
		n := a.Load()
		if n >= max {
			return false
		}
		if a.CompareAndSwap(n, n+1) {
			return true
		}
	}
}
func (b *nativeIngressBudget) acquire() bool {
	if !reserve(&nativeProcessActive, 262144) {
		b.rejected.Add(1)
		return false
	}
	if !reserve(&b.active, b.maxActive) {
		nativeProcessActive.Add(-1)
		b.rejected.Add(1)
		return false
	}
	if !reserve(&nativeProcessPending, 32768) {
		b.active.Add(-1)
		nativeProcessActive.Add(-1)
		b.rejected.Add(1)
		return false
	}
	if !reserve(&b.pending, b.maxPending) {
		nativeProcessPending.Add(-1)
		b.active.Add(-1)
		nativeProcessActive.Add(-1)
		b.rejected.Add(1)
		return false
	}
	return true
}
func (b *nativeIngressBudget) releasePending() { b.pending.Add(-1); nativeProcessPending.Add(-1) }
func (b *nativeIngressBudget) acquireUser(uid int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.users[uid] >= b.maxUser {
		b.rejected.Add(1)
		return false
	}
	b.users[uid]++
	return true
}
func (b *nativeIngressBudget) releaseUser(uid int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.users[uid]--
	if b.users[uid] <= 0 {
		delete(b.users, uid)
	}
}

func (b *nativeIngressBudget) allowPreauth(clientIP string, now time.Time) bool {
	if b == nil || b.preauthRate <= 0 || b.preauthBurst <= 0 {
		return true
	}
	key := clientIP
	if key == "" {
		key = "<unknown>"
	}
	b.ipMu.Lock()
	defer b.ipMu.Unlock()
	if now.Sub(b.ipLastCleanup) > time.Minute {
		for ip, bucket := range b.ipBuckets {
			if now.Sub(bucket.updated) > time.Minute {
				delete(b.ipBuckets, ip)
			}
		}
		b.ipLastCleanup = now
	}
	bucket, ok := b.ipBuckets[key]
	if !ok {
		if len(b.ipBuckets) >= 65536 {
			return false
		}
		bucket = nativeIPBucket{tokens: b.preauthBurst, updated: now}
	}
	if elapsed := now.Sub(bucket.updated).Seconds(); elapsed > 0 {
		bucket.tokens += elapsed * b.preauthRate
		if bucket.tokens > b.preauthBurst {
			bucket.tokens = b.preauthBurst
		}
		bucket.updated = now
	}
	if bucket.tokens < 1 {
		b.ipBuckets[key] = bucket
		return false
	}
	bucket.tokens--
	b.ipBuckets[key] = bucket
	return true
}

func (s *SntpEclipseServer) readCompatibleHello(conn net.Conn) (*sntpEclipseHandshakeState, error) {
	var prefix [50]byte
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		return nil, err
	}
	reader := io.MultiReader(bytes.NewReader(prefix[:]), conn)
	if titannative.IsV2Length(prefix[48:]) {
		return s.readClientHelloV2(&nativePrefixConn{Conn: conn, reader: reader})
	}
	state, err := titannative.Accept(reader, s.privateKey)
	if err != nil {
		return nil, err
	}
	hello, err := decodeSntpEclipseHello(state.Hello, 3)
	if err != nil {
		return nil, err
	}
	if math.Abs(time.Since(time.Unix(hello.Timestamp, 0)).Seconds()) > sntpEclipseHandshakeTTL.Seconds() {
		return nil, errors.New("expired native hello")
	}
	return &sntpEclipseHandshakeState{hello: hello, native: state, v2: true}, nil
}

// Unauthenticated peers receive no protocol reply. All early failures use the
// same bounded close delay; slow peers are constrained by the handshake deadline.
func (s *SntpEclipseServer) handleCompatibleConn(raw net.Conn) {
	b := s.nativeBudget
	pending := true
	accepted := false
	start := time.Now()
	jitter, _ := titannative.RandomPadding(150)
	defer func() {
		if !accepted {
			if wait := time.Until(start.Add(time.Duration(150+jitter) * time.Millisecond)); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-s.closed:
				}
				timer.Stop()
			}
		}
		_ = raw.Close()
		if pending {
			b.releasePending()
		}
		b.active.Add(-1)
		nativeProcessActive.Add(-1)
	}()
	_ = raw.SetDeadline(start.Add(10 * time.Second))
	prepared, clientIP, err := s.prepareConn(raw)
	if err != nil {
		b.invalid.Add(1)
		return
	}
	conn := prepared
	if !b.allowPreauth(clientIP, start) {
		b.preauthRejected.Add(1)
		return
	}
	hs, err := s.readCompatibleHello(conn)
	if err != nil {
		b.invalid.Add(1)
		return
	}
	hello := hs.hello
	if math.Abs(time.Since(time.Unix(hello.Timestamp, 0)).Seconds()) > sntpEclipseHandshakeTTL.Seconds() || (hello.Command != sntpEclipseCmdTCP && hello.Command != sntpEclipseCmdUDP) || (hello.Command == sntpEclipseCmdTCP && (hello.Target == "" || hello.Port == 0)) {
		b.invalid.Add(1)
		return
	}
	uid, ok := s.lookupUser(hello.UUID)
	if !ok {
		b.unauthorized.Add(1)
		return
	}
	if !b.acquireUser(uid) {
		return
	}
	defer b.releaseUser(uid)
	userTag := format.UserTag(s.tag, hello.UUID)
	if l, err := limiter.GetLimiter(s.tag); err == nil {
		if _, reject, _ := l.CheckLimit(userTag, clientIP, true); reject {
			b.unauthorized.Add(1)
			return
		}
	}
	if !s.replay.MarkScoped(hello.SessionID, uid, clientIP, time.Now()) {
		b.invalid.Add(1)
		b.replayRejected.Add(1)
		return
	}
	if err = s.writeServerReply(conn, hs, true); err != nil {
		b.invalid.Add(1)
		return
	}
	b.releasePending()
	pending = false
	accepted = true
	if hs.native != nil {
		b.acceptedV3.Add(1)
	} else {
		b.acceptedV2.Add(1)
	}
	_ = conn.SetDeadline(time.Time{})
	session := &sntpEclipseSession{nativeMetrics: b, conn: conn, clientIP: clientIP, hello: hello, c2s: hs.c2s, s2c: hs.s2c, tag: s.tag, userTag: userTag, uid: uid, xray: s.xray, dispatcher: s.dispatcher, serverName: s.tag, v2: hs.v2, onlineRefresh: s.onlineRefresh}
	if hello.Command == sntpEclipseCmdTCP {
		session.serveTCP()
	} else {
		session.serveUDP()
	}
}

// Runtime reports originate from the active listener, never desired config.
func TitanRuntimeStatus(tag string) map[string]any {
	value, ok := titanRuntimeServers.Load(tag)
	if !ok {
		return nil
	}
	s := value.(*SntpEclipseServer)
	select {
	case <-s.closed:
		return nil
	default:
	}
	sum := sha256.Sum256(s.privateKey.PublicKey().Bytes())
	modes := []string{s.mode}
	if s.mode == titannative.Mode {
		modes = []string{sntpEclipseModeV2, titannative.Mode}
	}
	_, portText, _ := net.SplitHostPort(s.listenAddr)
	port, _ := strconv.Atoi(portText)
	out := map[string]any{"modes": modes, "key_sha256": hex.EncodeToString(sum[:]), "server_port": port}
	if b := s.nativeBudget; b != nil && s.mode == titannative.Mode {
		out["pending"] = b.pending.Load()
		out["active"] = b.active.Load()
		out["capacity_rejected"] = b.rejected.Load()
		out["preauth_rejected"] = b.preauthRejected.Load()
		out["replay_rejected"] = b.replayRejected.Load()
		out["invalid"] = b.invalid.Load()
		out["dispatch_failed"] = b.dispatchFailed.Load()
		out["unauthorized"] = b.unauthorized.Load()
		out["accepted_v2"] = b.acceptedV2.Load()
		out["accepted_v3"] = b.acceptedV3.Load()
	}
	return out
}

type nativePrefixConn struct {
	net.Conn
	reader io.Reader
}

func (c *nativePrefixConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
