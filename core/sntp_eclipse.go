package core

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/limiter"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	sntpEclipseProtocol            = "sntp-eclipse"
	sntpEclipseModeV1              = "sntp-eclipse-v1"
	sntpEclipseModeV2              = "sntp-eclipse-v2"
	sntpEclipseVersionV1      byte = 1
	sntpEclipseVersionV2      byte = 2
	sntpEclipseCmdTCP         byte = 1
	sntpEclipseCmdUDP         byte = 2
	sntpEclipseHelloPlainSize      = 496
	sntpEclipseHelloSealSize       = sntpEclipseHelloPlainSize + chacha20poly1305.Overhead
	sntpEclipseReplyPlainSize      = 48
	sntpEclipseReplySealSize       = sntpEclipseReplyPlainSize + chacha20poly1305.Overhead
	sntpEclipseMaxFramePlain       = 16 * 1024
	sntpEclipseMaxFrameSeal        = sntpEclipseMaxFramePlain + chacha20poly1305.Overhead
	sntpEclipseHandshakeTTL        = 10 * time.Minute
)

var (
	sntpEclipseV2HelloPlainBuckets = []int{496, 752, 1008, 1264}
	sntpEclipseV2ReplyPlainBuckets = []int{48, 112, 240, 496}
	sntpEclipseV2FramePlainBuckets = []int{128, 512, 1024, 2048, 4096, 8192, 16384}
)

type SntpEclipseServer struct {
	tag                 string
	nodeID              int
	mode                string
	listenAddr          string
	acceptProxyProtocol bool
	privateKey          *ecdh.PrivateKey
	listener            net.Listener
	counter             *counter.TrafficCounter
	replay              *sntpEclipseReplayCache

	closeOnce sync.Once
	closed    chan struct{}
	usersMu   sync.RWMutex
	users     map[string]int
}

type sntpEclipseHello struct {
	Version   byte
	Command   byte
	Timestamp int64
	SessionID [16]byte
	UUID      string
	Target    string
	Port      uint16
}

type sntpEclipseSession struct {
	conn       net.Conn
	clientIP   string
	hello      sntpEclipseHello
	c2s        *sntpEclipseCipher
	s2c        *sntpEclipseCipher
	tag        string
	userTag    string
	uid        int
	counter    *counter.TrafficCounter
	limit      interface{ Wait(int64) }
	serverName string
	v2         bool
}

type sntpEclipseCipher struct {
	aead    cipherAEAD
	counter uint64
	mu      sync.Mutex
}

type sntpEclipseHandshakeState struct {
	hello          sntpEclipseHello
	c2s            *sntpEclipseCipher
	s2c            *sntpEclipseCipher
	replyCipher    *sntpEclipseCipher
	replyPublicKey []byte
	v2             bool
}

type sntpEclipseReplayCache struct {
	mu          sync.Mutex
	ttl         time.Duration
	seen        map[[32]byte]time.Time
	insertCount int
	lastCleanup time.Time
}

type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func newSntpEclipseServer(tag string, nodeInfo *panel.NodeInfo) (*SntpEclipseServer, error) {
	if nodeInfo == nil || nodeInfo.Common == nil {
		return nil, errors.New("missing node info")
	}
	privateKeyText := strings.TrimSpace(nodeInfo.Common.EncryptionSettings.PrivateKey)
	if privateKeyText == "" {
		return nil, errors.New("missing sntp-eclipse private key")
	}
	privateKeyBytes, err := decodeURLBase64(privateKeyText)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	mode := normalizeSntpEclipseMode(nodeInfo.Common.EncryptionSettings.Mode)
	if mode == "" {
		return nil, fmt.Errorf("unsupported sntp-eclipse mode: %s", nodeInfo.Common.EncryptionSettings.Mode)
	}
	listenIP := strings.TrimSpace(nodeInfo.Common.ListenIP)
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	listenAddr := net.JoinHostPort(listenIP, strconv.Itoa(nodeInfo.Common.ServerPort))
	acceptProxyProtocol, err := sntpEclipseAcceptProxyProtocol(nodeInfo)
	if err != nil {
		return nil, err
	}
	return &SntpEclipseServer{
		tag:                 tag,
		nodeID:              nodeInfo.Id,
		mode:                mode,
		listenAddr:          listenAddr,
		acceptProxyProtocol: acceptProxyProtocol,
		privateKey:          privateKey,
		counter:             counter.NewTrafficCounter(),
		replay:              newSntpEclipseReplayCache(sntpEclipseHandshakeTTL),
		closed:              make(chan struct{}),
		users:               make(map[string]int),
	}, nil
}

func (s *SntpEclipseServer) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return err
	}
	s.listener = ln
	go s.acceptLoop()
	log.WithFields(log.Fields{
		"tag":                   s.tag,
		"listen":                s.listenAddr,
		"mode":                  s.mode,
		"accept_proxy_protocol": s.acceptProxyProtocol,
	}).Info("SNTP Eclipse ingress started")
	return nil
}

func (s *SntpEclipseServer) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.listener != nil {
			err = s.listener.Close()
		}
	})
	return err
}

func (s *SntpEclipseServer) SetUsers(users []panel.UserInfo) {
	next := make(map[string]int, len(users))
	for _, user := range users {
		next[user.Uuid] = user.Id
	}
	s.usersMu.Lock()
	s.users = next
	s.usersMu.Unlock()
}

func (s *SntpEclipseServer) AddUsers(users []panel.UserInfo) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	for _, user := range users {
		s.users[user.Uuid] = user.Id
	}
}

func (s *SntpEclipseServer) DelUsers(users []panel.UserInfo) {
	s.usersMu.Lock()
	defer s.usersMu.Unlock()
	for _, user := range users {
		delete(s.users, user.Uuid)
		s.counter.Delete(format.UserTag(s.tag, user.Uuid))
	}
}

func (s *SntpEclipseServer) lookupUser(uuid string) (int, bool) {
	s.usersMu.RLock()
	uid, ok := s.users[uuid]
	s.usersMu.RUnlock()
	return uid, ok
}

func (s *SntpEclipseServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				log.WithFields(log.Fields{"tag": s.tag, "err": err}).Warn("SNTP Eclipse accept failed")
				continue
			}
		}
		go s.handleConn(conn)
	}
}

func (s *SntpEclipseServer) handleConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	conn, clientIP, err := s.prepareConn(conn)
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"err":       err,
		}).Warn("SNTP Eclipse proxy protocol failed")
		_ = conn.Close()
		return
	}
	handshake, err := s.readClientHello(conn)
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"err":       err,
		}).Warn("SNTP Eclipse handshake failed")
		_ = conn.Close()
		return
	}
	hello := handshake.hello
	uid, ok := s.lookupUser(hello.UUID)
	if !ok {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uuid":      maskSntpEclipseText(hello.UUID),
			"target":    hello.targetAddress(),
			"cmd":       hello.Command,
		}).Warn("SNTP Eclipse user not found")
		_ = s.writeServerReply(conn, handshake, false)
		_ = conn.Close()
		return
	}
	userTag := format.UserTag(s.tag, hello.UUID)
	var bucket interface{ Wait(int64) }
	if l, err := limiter.GetLimiter(s.tag); err == nil {
		if b, reject := l.CheckLimit(userTag, clientIP, true); reject {
			log.WithFields(log.Fields{
				"tag":       s.tag,
				"client_ip": clientIP,
				"uid":       uid,
				"target":    hello.targetAddress(),
				"cmd":       hello.Command,
			}).Warn("SNTP Eclipse user rejected by limiter")
			_ = s.writeServerReply(conn, handshake, false)
			_ = conn.Close()
			return
		} else if b != nil {
			bucket = b.Get()
		}
	}
	if err := s.writeServerReply(conn, handshake, true); err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uid":       uid,
			"target":    hello.targetAddress(),
			"err":       err,
		}).Warn("SNTP Eclipse reply failed")
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	session := &sntpEclipseSession{
		conn:       conn,
		clientIP:   clientIP,
		hello:      hello,
		c2s:        handshake.c2s,
		s2c:        handshake.s2c,
		tag:        s.tag,
		userTag:    userTag,
		uid:        uid,
		counter:    s.counter,
		limit:      bucket,
		serverName: s.tag,
		v2:         handshake.v2,
	}
	switch hello.Command {
	case sntpEclipseCmdTCP:
		session.serveTCP()
	case sntpEclipseCmdUDP:
		session.serveUDP()
	default:
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uid":       uid,
			"cmd":       hello.Command,
			"target":    hello.targetAddress(),
		}).Warn("SNTP Eclipse unsupported command")
		_ = conn.Close()
	}
}

func (s *SntpEclipseServer) readClientHello(conn net.Conn) (*sntpEclipseHandshakeState, error) {
	if s.mode == sntpEclipseModeV2 {
		return s.readClientHelloV2(conn)
	}
	return s.readClientHelloV1(conn)
}

func (s *SntpEclipseServer) readClientHelloV1(conn net.Conn) (*sntpEclipseHandshakeState, error) {
	var header [48]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	clientPublic, err := ecdh.X25519().NewPublicKey(header[:32])
	if err != nil {
		return nil, err
	}
	shared, err := s.privateKey.ECDH(clientPublic)
	if err != nil {
		return nil, err
	}
	c2s, s2c, err := newSntpEclipseCiphers(shared, header[32:])
	if err != nil {
		return nil, err
	}
	sealed := make([]byte, sntpEclipseHelloSealSize)
	if _, err := io.ReadFull(conn, sealed); err != nil {
		return nil, err
	}
	plain, err := c2s.openWithNonce(sealed, 0)
	if err != nil {
		return nil, err
	}
	hello, err := decodeSntpEclipseHello(plain, sntpEclipseVersionV1)
	if err != nil {
		return nil, err
	}
	c2s.counter = 1
	if math.Abs(time.Since(time.Unix(hello.Timestamp, 0)).Seconds()) > sntpEclipseHandshakeTTL.Seconds() {
		return nil, errors.New("expired hello")
	}
	return &sntpEclipseHandshakeState{hello: hello, c2s: c2s, s2c: s2c, replyCipher: s2c}, nil
}

func (s *SntpEclipseServer) readClientHelloV2(conn net.Conn) (*sntpEclipseHandshakeState, error) {
	var header [48]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	clientPublicBytes := append([]byte(nil), header[:32]...)
	clientPublic, err := ecdh.X25519().NewPublicKey(clientPublicBytes)
	if err != nil {
		return nil, err
	}
	authSecret, err := s.privateKey.ECDH(clientPublic)
	if err != nil {
		return nil, err
	}
	handshakeC2S, handshakeS2C, err := newSntpEclipseCiphersWithInfo(authSecret, header[32:], []byte("sntp-eclipse-v2-handshake"))
	if err != nil {
		return nil, err
	}
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	sealedLen := int(binary.BigEndian.Uint16(lenBuf[:]))
	if !isSntpEclipseSealBucket(sealedLen, sntpEclipseV2HelloPlainBuckets) {
		return nil, errors.New("invalid v2 hello size")
	}
	sealed := make([]byte, sealedLen)
	if _, err := io.ReadFull(conn, sealed); err != nil {
		return nil, err
	}
	plain, err := handshakeC2S.openWithNonce(sealed, 0)
	if err != nil {
		return nil, err
	}
	hello, err := decodeSntpEclipseHello(plain, sntpEclipseVersionV2)
	if err != nil {
		return nil, err
	}
	if math.Abs(time.Since(time.Unix(hello.Timestamp, 0)).Seconds()) > sntpEclipseHandshakeTTL.Seconds() {
		return nil, errors.New("expired hello")
	}
	if s.replay != nil && !s.replay.Mark(hello.SessionID, time.Now()) {
		return nil, errors.New("replayed hello")
	}
	serverEphemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	fsSecret, err := serverEphemeral.ECDH(clientPublic)
	if err != nil {
		return nil, err
	}
	serverPublicBytes := serverEphemeral.PublicKey().Bytes()
	transcript := sntpEclipseV2Transcript(clientPublicBytes, header[32:], lenBuf[:], sealed, serverPublicBytes)
	c2s, s2c, err := newSntpEclipseV2TrafficCiphers(authSecret, fsSecret, header[32:], transcript)
	if err != nil {
		return nil, err
	}
	return &sntpEclipseHandshakeState{
		hello:          hello,
		c2s:            c2s,
		s2c:            s2c,
		replyCipher:    handshakeS2C,
		replyPublicKey: serverPublicBytes,
		v2:             true,
	}, nil
}

func (s *SntpEclipseServer) writeServerReply(conn net.Conn, state *sntpEclipseHandshakeState, ok bool) error {
	if state != nil && state.v2 {
		return s.writeServerReplyV2(conn, state, ok)
	}
	return s.writeServerReplyV1(conn, state.replyCipher, ok)
}

func (s *SntpEclipseServer) writeServerReplyV1(conn net.Conn, s2c *sntpEclipseCipher, ok bool) error {
	plain := make([]byte, sntpEclipseReplyPlainSize)
	if ok {
		plain[0] = 1
	}
	if _, err := rand.Read(plain[1:]); err != nil {
		return err
	}
	sealed := s2c.sealWithNonce(plain, 0)
	_, err := conn.Write(sealed)
	s2c.counter = 1
	return err
}

func (s *SntpEclipseServer) writeServerReplyV2(conn net.Conn, state *sntpEclipseHandshakeState, ok bool) error {
	plainSize := chooseSntpEclipseBucket(1+32, sntpEclipseV2ReplyPlainBuckets)
	plain := make([]byte, plainSize)
	if ok {
		plain[0] = 1
	}
	copy(plain[1:33], state.replyPublicKey)
	if err := fillSntpEclipseRandom(plain[33:]); err != nil {
		return err
	}
	sealed := state.replyCipher.sealWithNonce(plain, 0)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(sealed)))
	if _, err := conn.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := conn.Write(sealed)
	return err
}

func (s *sntpEclipseSession) serveTCP() {
	target := net.JoinHostPort(s.hello.Target, strconv.Itoa(int(s.hello.Port)))
	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", target)
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"uid":       s.uid,
			"target":    target,
			"client_ip": s.clientIP,
			"err":       err,
		}).Warn("SNTP Eclipse upstream dial failed")
		_ = s.conn.Close()
		return
	}
	log.WithFields(log.Fields{
		"tag":       s.tag,
		"uid":       s.uid,
		"target":    target,
		"client_ip": s.clientIP,
	}).Info("SNTP Eclipse TCP session accepted")
	defer upstream.Close()
	defer s.conn.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer s.conn.Close()
		s.copyClientToTarget(upstream)
		if tcpConn, ok := upstream.(*net.TCPConn); ok {
			_ = tcpConn.CloseWrite()
		} else {
			_ = upstream.Close()
		}
	}()
	go func() {
		defer wg.Done()
		defer s.conn.Close()
		s.copyTargetToClient(upstream)
	}()
	wg.Wait()
}

func (s *sntpEclipseSession) copyClientToTarget(dst net.Conn) {
	for {
		frame, err := s.readFrame(s.c2s)
		if err != nil {
			return
		}
		if len(frame) == 0 {
			continue
		}
		if s.limit != nil {
			s.limit.Wait(int64(len(frame)))
		}
		n, err := dst.Write(frame)
		if n > 0 {
			s.counter.Tx(s.userTag, n)
		}
		if err != nil {
			return
		}
	}
}

func (s *sntpEclipseSession) copyTargetToClient(src net.Conn) {
	buf := make([]byte, s.maxFramePayload())
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if s.limit != nil {
				s.limit.Wait(int64(n))
			}
			if werr := s.writeFrame(s.s2c, buf[:n]); werr != nil {
				return
			}
			s.counter.Rx(s.userTag, n)
		}
		if err != nil {
			return
		}
	}
}

func (s *sntpEclipseSession) serveUDP() {
	defer s.conn.Close()
	for {
		frame, err := s.readFrame(s.c2s)
		if err != nil {
			return
		}
		target, payload, err := decodeSntpEclipsePacket(frame)
		if err != nil || len(payload) == 0 {
			continue
		}
		if s.limit != nil {
			s.limit.Wait(int64(len(payload)))
		}
		s.counter.Tx(s.userTag, len(payload))
		response, err := exchangeUDP(target, payload)
		if err != nil || len(response) == 0 {
			continue
		}
		if s.limit != nil {
			s.limit.Wait(int64(len(response)))
		}
		s.counter.Rx(s.userTag, len(response))
		_ = s.writeFrame(s.s2c, encodeSntpEclipsePacket(target, response))
	}
}

func exchangeUDP(target string, payload []byte) ([]byte, error) {
	conn, err := net.DialTimeout("udp", target, 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64*1024)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (s *sntpEclipseSession) readFrame(c *sntpEclipseCipher) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(s.conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n <= chacha20poly1305.Overhead || n > sntpEclipseMaxFrameSeal {
		return nil, errors.New("invalid frame length")
	}
	sealed := make([]byte, n)
	if _, err := io.ReadFull(s.conn, sealed); err != nil {
		return nil, err
	}
	plain, err := c.Open(sealed)
	if err != nil {
		return nil, err
	}
	if s.v2 {
		return decodeSntpEclipseV2Frame(plain)
	}
	return plain, nil
}

func (s *sntpEclipseSession) writeFrame(c *sntpEclipseCipher, plain []byte) error {
	if s.v2 {
		wrapped, err := encodeSntpEclipseV2Frame(plain)
		if err != nil {
			return err
		}
		plain = wrapped
	}
	if len(plain) > sntpEclipseMaxFramePlain {
		return errors.New("frame too large")
	}
	sealed := c.Seal(plain)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(sealed)))
	if _, err := s.conn.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := s.conn.Write(sealed)
	return err
}

func (s *sntpEclipseSession) maxFramePayload() int {
	if s.v2 {
		return sntpEclipseMaxFramePlain - 2
	}
	return sntpEclipseMaxFramePlain
}

func newSntpEclipseCiphers(shared, salt []byte) (*sntpEclipseCipher, *sntpEclipseCipher, error) {
	return newSntpEclipseCiphersWithInfo(shared, salt, []byte(sntpEclipseModeV1))
}

func newSntpEclipseCiphersWithInfo(shared, salt, info []byte) (*sntpEclipseCipher, *sntpEclipseCipher, error) {
	reader := hkdf.New(sha256.New, shared, salt, info)
	keys := make([]byte, chacha20poly1305.KeySize*2)
	if _, err := io.ReadFull(reader, keys); err != nil {
		return nil, nil, err
	}
	c2sAEAD, err := chacha20poly1305.NewX(keys[:chacha20poly1305.KeySize])
	if err != nil {
		return nil, nil, err
	}
	s2cAEAD, err := chacha20poly1305.NewX(keys[chacha20poly1305.KeySize:])
	if err != nil {
		return nil, nil, err
	}
	return &sntpEclipseCipher{aead: c2sAEAD}, &sntpEclipseCipher{aead: s2cAEAD}, nil
}

func newSntpEclipseV2TrafficCiphers(authSecret, fsSecret, salt, transcript []byte) (*sntpEclipseCipher, *sntpEclipseCipher, error) {
	keyMaterial := make([]byte, 0, len(authSecret)+len(fsSecret)+len(transcript))
	keyMaterial = append(keyMaterial, authSecret...)
	keyMaterial = append(keyMaterial, fsSecret...)
	keyMaterial = append(keyMaterial, transcript...)
	return newSntpEclipseCiphersWithInfo(keyMaterial, salt, []byte("sntp-eclipse-v2-traffic"))
}

func (c *sntpEclipseCipher) Seal(plain []byte) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	sealed := c.sealWithNonce(plain, c.counter)
	c.counter++
	return sealed
}

func (c *sntpEclipseCipher) Open(sealed []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	plain, err := c.openWithNonce(sealed, c.counter)
	if err != nil {
		return nil, err
	}
	c.counter++
	return plain, nil
}

func (c *sntpEclipseCipher) sealWithNonce(plain []byte, counter uint64) []byte {
	nonce := make([]byte, c.aead.NonceSize())
	binary.BigEndian.PutUint64(nonce[len(nonce)-8:], counter)
	return c.aead.Seal(nil, nonce, plain, nil)
}

func (c *sntpEclipseCipher) openWithNonce(sealed []byte, counter uint64) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	binary.BigEndian.PutUint64(nonce[len(nonce)-8:], counter)
	return c.aead.Open(nil, nonce, sealed, nil)
}

func encodeSntpEclipsePacket(target string, payload []byte) []byte {
	targetBytes := []byte(target)
	buf := make([]byte, 2+len(targetBytes)+len(payload))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(targetBytes)))
	copy(buf[2:], targetBytes)
	copy(buf[2+len(targetBytes):], payload)
	return buf
}

func decodeSntpEclipsePacket(frame []byte) (string, []byte, error) {
	if len(frame) < 2 {
		return "", nil, errors.New("short packet")
	}
	targetLen := int(binary.BigEndian.Uint16(frame[:2]))
	if targetLen <= 0 || 2+targetLen > len(frame) {
		return "", nil, errors.New("invalid target length")
	}
	return string(frame[2 : 2+targetLen]), frame[2+targetLen:], nil
}

func decodeSntpEclipseHello(plain []byte, wantVersion byte) (sntpEclipseHello, error) {
	if wantVersion == sntpEclipseVersionV1 && len(plain) != sntpEclipseHelloPlainSize {
		return sntpEclipseHello{}, errors.New("invalid hello size")
	}
	if len(plain) < sntpEclipseHelloPlainSize || len(plain) > sntpEclipseV2HelloPlainBuckets[len(sntpEclipseV2HelloPlainBuckets)-1] {
		return sntpEclipseHello{}, errors.New("invalid hello size")
	}
	payloadLen := int(binary.BigEndian.Uint16(plain[:2]))
	if payloadLen <= 0 || payloadLen > len(plain)-2 {
		return sntpEclipseHello{}, errors.New("invalid hello payload")
	}
	p := plain[2 : 2+payloadLen]
	if len(p) < 13 || p[0] != wantVersion {
		return sntpEclipseHello{}, errors.New("invalid hello version")
	}
	version := p[0]
	offset := 10
	cmd := p[1]
	var sessionID [16]byte
	if version == sntpEclipseVersionV2 {
		if len(p) < offset+16+3 {
			return sntpEclipseHello{}, errors.New("invalid session id")
		}
		copy(sessionID[:], p[offset:offset+16])
		if isZeroSntpEclipseSessionID(sessionID) {
			return sntpEclipseHello{}, errors.New("empty session id")
		}
		offset += 16
	}
	uuidLen := int(p[offset])
	offset++
	if uuidLen <= 0 || offset+uuidLen > len(p) {
		return sntpEclipseHello{}, errors.New("invalid uuid")
	}
	uuid := string(p[offset : offset+uuidLen])
	offset += uuidLen
	if offset+3 > len(p) {
		return sntpEclipseHello{}, errors.New("invalid target")
	}
	targetLen := int(binary.BigEndian.Uint16(p[offset : offset+2]))
	offset += 2
	if targetLen < 0 || offset+targetLen+2 > len(p) {
		return sntpEclipseHello{}, errors.New("invalid target length")
	}
	target := string(p[offset : offset+targetLen])
	offset += targetLen
	port := binary.BigEndian.Uint16(p[offset : offset+2])
	timestamp := int64(binary.BigEndian.Uint64(p[2:10]))
	return sntpEclipseHello{Version: version, Command: cmd, Timestamp: timestamp, SessionID: sessionID, UUID: uuid, Target: target, Port: port}, nil
}

func (h sntpEclipseHello) targetAddress() string {
	if h.Target == "" || h.Port == 0 {
		return ""
	}
	return net.JoinHostPort(h.Target, strconv.Itoa(int(h.Port)))
}

func encodeSntpEclipseV2Frame(payload []byte) ([]byte, error) {
	minSize := 2 + len(payload)
	if minSize > sntpEclipseMaxFramePlain {
		return nil, errors.New("frame too large")
	}
	plainSize := chooseSntpEclipseBucket(minSize, sntpEclipseV2FramePlainBuckets)
	plain := make([]byte, plainSize)
	binary.BigEndian.PutUint16(plain[:2], uint16(len(payload)))
	copy(plain[2:], payload)
	if err := fillSntpEclipseRandom(plain[2+len(payload):]); err != nil {
		return nil, err
	}
	return plain, nil
}

func decodeSntpEclipseV2Frame(plain []byte) ([]byte, error) {
	if len(plain) < 2 {
		return nil, errors.New("short v2 frame")
	}
	payloadLen := int(binary.BigEndian.Uint16(plain[:2]))
	if payloadLen > len(plain)-2 {
		return nil, errors.New("invalid v2 frame payload")
	}
	return plain[2 : 2+payloadLen], nil
}

func maskSntpEclipseText(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:6] + "..." + value[len(value)-6:]
}

func normalizeSntpEclipseMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "", sntpEclipseModeV1:
		return sntpEclipseModeV1
	case sntpEclipseModeV2:
		return sntpEclipseModeV2
	default:
		return ""
	}
}

func isSntpEclipseSealBucket(size int, plainBuckets []int) bool {
	for _, bucket := range plainBuckets {
		if size == bucket+chacha20poly1305.Overhead {
			return true
		}
	}
	return false
}

func chooseSntpEclipseBucket(minSize int, buckets []int) int {
	candidates := make([]int, 0, 3)
	for _, bucket := range buckets {
		if bucket >= minSize {
			candidates = append(candidates, bucket)
			if len(candidates) >= 3 {
				break
			}
		}
	}
	if len(candidates) == 0 {
		return minSize
	}
	var b [1]byte
	if _, err := rand.Read(b[:]); err != nil {
		return candidates[0]
	}
	return candidates[int(b[0])%len(candidates)]
}

func fillSntpEclipseRandom(buf []byte) error {
	if len(buf) == 0 {
		return nil
	}
	_, err := rand.Read(buf)
	return err
}

func isZeroSntpEclipseSessionID(sessionID [16]byte) bool {
	for _, b := range sessionID {
		if b != 0 {
			return false
		}
	}
	return true
}

func sntpEclipseV2Transcript(clientPublic, salt, lenBuf, sealedHello, serverPublic []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("sntp-eclipse-v2-transcript"))
	_, _ = h.Write(clientPublic)
	_, _ = h.Write(salt)
	_, _ = h.Write(lenBuf)
	_, _ = h.Write(sealedHello)
	_, _ = h.Write(serverPublic)
	return h.Sum(nil)
}

func newSntpEclipseReplayCache(ttl time.Duration) *sntpEclipseReplayCache {
	return &sntpEclipseReplayCache{
		ttl:         ttl,
		seen:        make(map[[32]byte]time.Time),
		lastCleanup: time.Now(),
	}
}

func (c *sntpEclipseReplayCache) Mark(sessionID [16]byte, now time.Time) bool {
	if c == nil {
		return true
	}
	key := sha256.Sum256(sessionID[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	if expiresAt, ok := c.seen[key]; ok && now.Before(expiresAt) {
		return false
	}
	c.seen[key] = now.Add(c.ttl)
	c.insertCount++
	if c.insertCount%256 == 0 || now.Sub(c.lastCleanup) > time.Minute {
		for k, expiresAt := range c.seen {
			if !now.Before(expiresAt) {
				delete(c.seen, k)
			}
		}
		c.lastCleanup = now
	}
	return true
}

func decodeURLBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("empty base64")
	}
	value = strings.ReplaceAll(value, "-", "+")
	value = strings.ReplaceAll(value, "_", "/")
	if pad := len(value) % 4; pad != 0 {
		value += strings.Repeat("=", 4-pad)
	}
	return base64.StdEncoding.DecodeString(value)
}

type sntpEclipseBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *sntpEclipseBufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func sntpEclipseAcceptProxyProtocol(nodeInfo *panel.NodeInfo) (bool, error) {
	if nodeInfo == nil || nodeInfo.Common == nil || len(nodeInfo.Common.NetworkSettings) == 0 {
		return false, nil
	}
	settings := &NetworkSettingsProxyProtocol{}
	if err := json.Unmarshal(nodeInfo.Common.NetworkSettings, settings); err != nil {
		return false, fmt.Errorf("unmarshal network settings error: %w", err)
	}
	return settings.AcceptProxyProtocol, nil
}

func (s *SntpEclipseServer) prepareConn(conn net.Conn) (net.Conn, string, error) {
	clientIP := remoteIP(conn.RemoteAddr())
	if !s.acceptProxyProtocol {
		return conn, clientIP, nil
	}
	reader := bufio.NewReader(conn)
	proxyIP, err := readProxyProtocolClientIP(reader)
	if err != nil {
		return conn, clientIP, err
	}
	if proxyIP != "" {
		clientIP = proxyIP
	}
	return &sntpEclipseBufferedConn{Conn: conn, reader: reader}, clientIP, nil
}

func readProxyProtocolClientIP(reader *bufio.Reader) (string, error) {
	first, err := reader.Peek(1)
	if err != nil {
		return "", err
	}
	switch first[0] {
	case 'P':
		prefix, err := reader.Peek(6)
		if err != nil {
			return "", err
		}
		if string(prefix) != "PROXY " {
			return "", nil
		}
		return readProxyProtocolV1ClientIP(reader)
	case '\r':
		header, err := reader.Peek(12)
		if err != nil {
			return "", err
		}
		if string(header) != "\r\n\r\n\x00\r\nQUIT\n" {
			return "", nil
		}
		return readProxyProtocolV2ClientIP(reader)
	default:
		return "", nil
	}
}

func readProxyProtocolV1ClientIP(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > 108 {
		return "", errors.New("proxy protocol v1 header too long")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "PROXY" {
		return "", errors.New("invalid proxy protocol v1 header")
	}
	if fields[1] == "UNKNOWN" {
		return "", nil
	}
	if len(fields) < 6 {
		return "", errors.New("invalid proxy protocol v1 address fields")
	}
	sourceIP := net.ParseIP(fields[2])
	if sourceIP == nil {
		return "", errors.New("invalid proxy protocol v1 source ip")
	}
	return strings.TrimPrefix(sourceIP.String(), "::ffff:"), nil
}

func readProxyProtocolV2ClientIP(reader *bufio.Reader) (string, error) {
	header := make([]byte, 16)
	if _, err := io.ReadFull(reader, header); err != nil {
		return "", err
	}
	if string(header[:12]) != "\r\n\r\n\x00\r\nQUIT\n" {
		return "", errors.New("invalid proxy protocol v2 signature")
	}
	if header[12]>>4 != 2 {
		return "", errors.New("invalid proxy protocol v2 version")
	}
	command := header[12] & 0x0f
	if command == 0 {
		return "", nil
	}
	if command != 1 {
		return "", errors.New("invalid proxy protocol v2 command")
	}
	addressLen := int(binary.BigEndian.Uint16(header[14:16]))
	body := make([]byte, addressLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		return "", err
	}
	switch header[13] {
	case 0x11: // TCP over IPv4.
		if len(body) < 12 {
			return "", errors.New("short proxy protocol v2 ipv4 address")
		}
		return net.IP(body[:4]).String(), nil
	case 0x21: // TCP over IPv6.
		if len(body) < 36 {
			return "", errors.New("short proxy protocol v2 ipv6 address")
		}
		return strings.TrimPrefix(net.IP(body[:16]).String(), "::ffff:"), nil
	default:
		return "", nil
	}
}

func remoteIP(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return strings.TrimPrefix(host, "::ffff:")
}

func isSntpEclipseNode(info *panel.NodeInfo) bool {
	return info != nil && info.Type == sntpEclipseProtocol
}

func isSntpEclipseProtocol(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), sntpEclipseProtocol)
}
