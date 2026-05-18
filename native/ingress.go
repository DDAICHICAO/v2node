package native

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

type TokenVerifier interface {
	VerifyTransportToken(ctx context.Context, token string) (*panel.TransportTokenVerifyResult, error)
}

type Ingress struct {
	tag             string
	verifier        TokenVerifier
	acceptedNodeIDs map[string]struct{}
	tcp             net.Listener
	udp             *net.UDPConn
	done            chan struct{}
	closeMu         sync.Once
	wg              sync.WaitGroup
}

type connectRequest struct {
	Version     int    `json:"version"`
	Command     string `json:"command"`
	Network     string `json:"network"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	NodeID      string `json:"node_id"`
	AccessToken string `json:"access_token"`
}

const maxUDPFrameSize = 65535

func Start(tag string, info *panel.NodeInfo, verifier TokenVerifier) (*Ingress, error) {
	if info == nil || info.Common == nil {
		return nil, fmt.Errorf("empty sntp-native node info")
	}
	if verifier == nil {
		return nil, fmt.Errorf("empty sntp-native token verifier")
	}

	listenIP := info.Common.ListenIP
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	address := net.JoinHostPort(listenIP, strconv.Itoa(info.Common.ServerPort))
	tcp, err := listenTCP(address, info)
	if err != nil {
		return nil, fmt.Errorf("listen tcp %s: %w", address, err)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("resolve udp %s: %w", address, err)
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("listen udp %s: %w", address, err)
	}

	ingress := &Ingress{
		tag:             tag,
		verifier:        verifier,
		acceptedNodeIDs: acceptedNativeNodeIDs(info),
		tcp:             tcp,
		udp:             udp,
		done:            make(chan struct{}),
	}
	ingress.wg.Add(2)
	go ingress.acceptTCP()
	go ingress.readUDP()
	log.WithFields(log.Fields{"tag": tag, "listen": address}).Info("SNTP native ingress started")
	return ingress, nil
}

func (i *Ingress) Close() error {
	i.closeMu.Do(func() {
		close(i.done)
		if i.tcp != nil {
			_ = i.tcp.Close()
		}
		if i.udp != nil {
			_ = i.udp.Close()
		}
		i.wg.Wait()
	})
	return nil
}

func (i *Ingress) acceptTCP() {
	defer i.wg.Done()
	for {
		conn, err := i.tcp.Accept()
		if err != nil {
			select {
			case <-i.done:
				return
			default:
				log.WithFields(log.Fields{"tag": i.tag, "err": err}).Warn("SNTP native tcp accept failed")
				continue
			}
		}
		i.wg.Add(1)
		go i.handleTCP(conn)
	}
}

func (i *Ingress) handleTCP(conn net.Conn) {
	defer i.wg.Done()
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	request, reader, err := readConnectRequest(conn)
	if err != nil {
		i.writeError(conn, "BAD_REQUEST")
		log.WithFields(log.Fields{"tag": i.tag, "err": err}).Debug("SNTP native bad tcp request")
		return
	}
	if err := i.verifyToken(request); err != nil {
		i.writeError(conn, "AUTH_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "err": err}).Warn("SNTP native token verify failed")
		return
	}

	if request.Network == "udp" {
		i.handleUDPSession(conn, reader, request)
		return
	}

	target := net.JoinHostPort(request.Host, strconv.Itoa(request.Port))
	upstream, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		i.writeError(conn, "DIAL_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "target": target, "err": err}).Debug("SNTP native target dial failed")
		return
	}
	defer upstream.Close()

	_ = conn.SetReadDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte("SNTP_NATIVE_V1 OK\n")); err != nil {
		return
	}
	i.pipeTCP(conn, reader, upstream)
}

func (i *Ingress) readUDP() {
	defer i.wg.Done()
	buf := make([]byte, 2048)
	for {
		_, addr, err := i.udp.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-i.done:
				return
			default:
				log.WithFields(log.Fields{"tag": i.tag, "err": err}).Warn("SNTP native udp read failed")
				continue
			}
		}
		_, _ = i.udp.WriteToUDP([]byte("SNTP_NATIVE_V1 ERROR NOT_IMPLEMENTED\n"), addr)
	}
}

func listenTCP(address string, info *panel.NodeInfo) (net.Listener, error) {
	if info.Security != panel.Tls {
		return net.Listen("tcp", address)
	}

	certInfo := info.Common.CertInfo
	if certInfo == nil || certInfo.CertFile == "" || certInfo.KeyFile == "" {
		log.WithField("listen", address).Warn("SNTP native TLS requested without cert files; falling back to plain TCP")
		return net.Listen("tcp", address)
	}
	if _, err := os.Stat(certInfo.CertFile); err != nil {
		log.WithFields(log.Fields{"listen": address, "cert": certInfo.CertFile, "err": err}).Warn("SNTP native cert missing; falling back to plain TCP")
		return net.Listen("tcp", address)
	}
	if _, err := os.Stat(certInfo.KeyFile); err != nil {
		log.WithFields(log.Fields{"listen": address, "key": certInfo.KeyFile, "err": err}).Warn("SNTP native key missing; falling back to plain TCP")
		return net.Listen("tcp", address)
	}

	cert, err := tls.LoadX509KeyPair(certInfo.CertFile, certInfo.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load tls cert: %w", err)
	}
	return tls.Listen("tcp", address, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"sntp-native/1"},
	})
}

func readConnectRequest(conn net.Conn) (connectRequest, *bufio.Reader, error) {
	reader := bufio.NewReaderSize(conn, 64*1024)
	first, err := reader.Peek(1)
	if err != nil {
		return connectRequest{}, reader, err
	}
	if len(first) != 1 || first[0] != 'S' {
		return connectRequest{}, reader, errors.New("invalid protocol prefix")
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return connectRequest{}, reader, err
	}
	line = strings.TrimSpace(line)
	payload, ok := strings.CutPrefix(line, "SNTP_NATIVE_V1 ")
	if !ok {
		return connectRequest{}, reader, errors.New("missing protocol prefix")
	}

	var request connectRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return connectRequest{}, reader, err
	}
	if err := validateConnectRequest(request); err != nil {
		return connectRequest{}, reader, err
	}
	return request, reader, nil
}

func validateConnectRequest(request connectRequest) error {
	if request.Version != 1 {
		return fmt.Errorf("unsupported version %d", request.Version)
	}
	if !((request.Command == "connect" && request.Network == "tcp") ||
		(request.Command == "udp" && request.Network == "udp")) {
		return fmt.Errorf("unsupported command %s/%s", request.Command, request.Network)
	}
	if request.Host == "" || strings.ContainsAny(request.Host, "\x00\r\n") {
		return errors.New("invalid host")
	}
	if request.Port <= 0 || request.Port > 65535 {
		return errors.New("invalid port")
	}
	if request.AccessToken == "" {
		return errors.New("missing access token")
	}
	return nil
}

func (i *Ingress) verifyToken(request connectRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := i.verifier.VerifyTransportToken(ctx, request.AccessToken)
	if err != nil {
		return err
	}
	if result == nil || !result.Valid {
		code := "UNKNOWN"
		if result != nil {
			code = result.Code
		}
		return fmt.Errorf("panel rejected token: %s", code)
	}
	if request.NodeID != "" && result.NodeID != "" && request.NodeID != result.NodeID {
		log.WithFields(log.Fields{
			"tag":      i.tag,
			"request":  request.NodeID,
			"verified": result.NodeID,
		}).Debug("SNTP native logical node differs from verified auth node")
	}
	if result.NodeID != "" && len(i.acceptedNodeIDs) > 0 {
		if _, ok := i.acceptedNodeIDs[result.NodeID]; !ok {
			return fmt.Errorf("node not accepted by this ingress: %s", result.NodeID)
		}
	}
	return nil
}

func acceptedNativeNodeIDs(info *panel.NodeInfo) map[string]struct{} {
	accepted := make(map[string]struct{})
	if info != nil && info.Common != nil {
		for _, id := range info.Common.NativeAcceptedNodeIDs {
			id = strings.ToLower(strings.TrimSpace(id))
			if id != "" {
				accepted[id] = struct{}{}
			}
		}
	}
	if len(accepted) == 0 && info != nil && info.Id > 0 {
		accepted[fmt.Sprintf("v2node-%d", info.Id)] = struct{}{}
	}
	return accepted
}

func (i *Ingress) pipeTCP(client net.Conn, clientReader *bufio.Reader, upstream net.Conn) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, clientReader)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeBoth()
	}()
	wg.Wait()
}

func (i *Ingress) handleUDPSession(conn net.Conn, reader *bufio.Reader, request connectRequest) {
	target := net.JoinHostPort(request.Host, strconv.Itoa(request.Port))
	udpAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		i.writeError(conn, "UDP_RESOLVE_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "target": target, "err": err}).Debug("SNTP native udp resolve failed")
		return
	}
	upstream, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		i.writeError(conn, "UDP_DIAL_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "target": target, "err": err}).Debug("SNTP native udp dial failed")
		return
	}
	defer upstream.Close()

	_ = conn.SetReadDeadline(time.Time{})
	if _, err := conn.Write([]byte("SNTP_NATIVE_V1 OK\n")); err != nil {
		return
	}
	i.pipeUDP(conn, reader, upstream)
}

func (i *Ingress) pipeUDP(client net.Conn, clientReader *bufio.Reader, upstream *net.UDPConn) {
	var writeMu sync.Mutex
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			payload, err := readFrame(clientReader)
			if err != nil {
				closeBoth()
				return
			}
			if _, err := upstream.Write(payload); err != nil {
				closeBoth()
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, maxUDPFrameSize)
		for {
			n, err := upstream.Read(buf)
			if err != nil {
				closeBoth()
				return
			}
			writeMu.Lock()
			err = writeFrame(client, buf[:n])
			writeMu.Unlock()
			if err != nil {
				closeBoth()
				return
			}
		}
	}()
	wg.Wait()
}

func readFrame(reader *bufio.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxUDPFrameSize {
		return nil, fmt.Errorf("invalid udp frame length %d", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeFrame(writer io.Writer, payload []byte) error {
	if len(payload) == 0 || len(payload) > maxUDPFrameSize {
		return fmt.Errorf("invalid udp frame length %d", len(payload))
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(payload)
	return err
}

func (i *Ingress) writeError(conn net.Conn, code string) {
	_, _ = conn.Write([]byte("SNTP_NATIVE_V1 ERROR " + code + "\n"))
}
