package core

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
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
	sntpEclipseVersion        byte = 1
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

type SntpEclipseServer struct {
	tag        string
	nodeID     int
	listenAddr string
	privateKey *ecdh.PrivateKey
	listener   net.Listener
	counter    *counter.TrafficCounter

	closeOnce sync.Once
	closed    chan struct{}
	usersMu   sync.RWMutex
	users     map[string]int
}

type sntpEclipseHello struct {
	Command   byte
	Timestamp int64
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
}

type sntpEclipseCipher struct {
	aead    cipherAEAD
	counter uint64
	mu      sync.Mutex
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
	listenIP := strings.TrimSpace(nodeInfo.Common.ListenIP)
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	listenAddr := net.JoinHostPort(listenIP, strconv.Itoa(nodeInfo.Common.ServerPort))
	return &SntpEclipseServer{
		tag:        tag,
		nodeID:     nodeInfo.Id,
		listenAddr: listenAddr,
		privateKey: privateKey,
		counter:    counter.NewTrafficCounter(),
		closed:     make(chan struct{}),
		users:      make(map[string]int),
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
		"tag":    s.tag,
		"listen": s.listenAddr,
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
	clientIP := remoteIP(conn.RemoteAddr())
	hello, c2s, s2c, err := s.readClientHello(conn)
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"err":       err,
		}).Warn("SNTP Eclipse handshake failed")
		_ = conn.Close()
		return
	}
	uid, ok := s.lookupUser(hello.UUID)
	if !ok {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uuid":      maskSntpEclipseText(hello.UUID),
			"target":    hello.targetAddress(),
			"cmd":       hello.Command,
		}).Warn("SNTP Eclipse user not found")
		_ = s.writeServerReply(conn, s2c, false)
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
			_ = s.writeServerReply(conn, s2c, false)
			_ = conn.Close()
			return
		} else if b != nil {
			bucket = b.Get()
		}
	}
	if err := s.writeServerReply(conn, s2c, true); err != nil {
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
		c2s:        c2s,
		s2c:        s2c,
		tag:        s.tag,
		userTag:    userTag,
		uid:        uid,
		counter:    s.counter,
		limit:      bucket,
		serverName: s.tag,
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

func (s *SntpEclipseServer) readClientHello(conn net.Conn) (sntpEclipseHello, *sntpEclipseCipher, *sntpEclipseCipher, error) {
	var header [48]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	clientPublic, err := ecdh.X25519().NewPublicKey(header[:32])
	if err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	shared, err := s.privateKey.ECDH(clientPublic)
	if err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	c2s, s2c, err := newSntpEclipseCiphers(shared, header[32:])
	if err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	sealed := make([]byte, sntpEclipseHelloSealSize)
	if _, err := io.ReadFull(conn, sealed); err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	plain, err := c2s.openWithNonce(sealed, 0)
	if err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	hello, err := decodeSntpEclipseHello(plain)
	if err != nil {
		return sntpEclipseHello{}, nil, nil, err
	}
	c2s.counter = 1
	if math.Abs(time.Since(time.Unix(hello.Timestamp, 0)).Seconds()) > sntpEclipseHandshakeTTL.Seconds() {
		return sntpEclipseHello{}, nil, nil, errors.New("expired hello")
	}
	return hello, c2s, s2c, nil
}

func (s *SntpEclipseServer) writeServerReply(conn net.Conn, s2c *sntpEclipseCipher, ok bool) error {
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
	buf := make([]byte, sntpEclipseMaxFramePlain)
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
	return c.Open(sealed)
}

func (s *sntpEclipseSession) writeFrame(c *sntpEclipseCipher, plain []byte) error {
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

func newSntpEclipseCiphers(shared, salt []byte) (*sntpEclipseCipher, *sntpEclipseCipher, error) {
	reader := hkdf.New(sha256.New, shared, salt, []byte("sntp-eclipse-v1"))
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

func decodeSntpEclipseHello(plain []byte) (sntpEclipseHello, error) {
	if len(plain) != sntpEclipseHelloPlainSize {
		return sntpEclipseHello{}, errors.New("invalid hello size")
	}
	payloadLen := int(binary.BigEndian.Uint16(plain[:2]))
	if payloadLen <= 0 || payloadLen > len(plain)-2 {
		return sntpEclipseHello{}, errors.New("invalid hello payload")
	}
	p := plain[2 : 2+payloadLen]
	if len(p) < 13 || p[0] != sntpEclipseVersion {
		return sntpEclipseHello{}, errors.New("invalid hello version")
	}
	offset := 10
	cmd := p[1]
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
	return sntpEclipseHello{Command: cmd, Timestamp: timestamp, UUID: uuid, Target: target, Port: port}, nil
}

func (h sntpEclipseHello) targetAddress() string {
	if h.Target == "" || h.Port == 0 {
		return ""
	}
	return net.JoinHostPort(h.Target, strconv.Itoa(int(h.Port)))
}

func maskSntpEclipseText(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:6] + "..." + value[len(value)-6:]
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
