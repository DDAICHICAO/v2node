package native

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"net"
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
	trafficMu       sync.Mutex
	trafficByUID    map[int]*trafficStats
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
		trafficByUID:    make(map[int]*trafficStats),
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
	prepareTCPConn(conn)
	defer gracefulClose(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	request, reader, err := readConnectRequest(conn)
	if err != nil {
		i.writeError(conn, "BAD_REQUEST")
		log.WithFields(log.Fields{"tag": i.tag, "err": err}).Debug("SNTP native bad tcp request")
		return
	}
	token, err := i.verifyToken(request)
	if err != nil {
		i.writeError(conn, "AUTH_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "err": err}).Warn("SNTP native token verify failed")
		return
	}

	if request.Network == "udp" {
		i.handleUDPSession(conn, reader, request, token)
		return
	}

	target := net.JoinHostPort(request.Host, strconv.Itoa(request.Port))
	upstream, err := dialTCPTarget(request.Host, request.Port, 15*time.Second)
	if err != nil {
		i.writeError(conn, "DIAL_FAILED")
		log.WithFields(log.Fields{"tag": i.tag, "target": target, "err": err}).Debug("SNTP native target dial failed")
		return
	}
	prepareTCPConn(upstream)
	defer gracefulClose(upstream)

	_ = conn.SetReadDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	if _, err := conn.Write([]byte("SNTP_NATIVE_V1 OK\n")); err != nil {
		return
	}
	i.logAcceptedSession("tcp", target, upstream.RemoteAddr().String(), request, token)
	stats := i.pipeTCP(conn, reader, upstream, token)
	i.logFinishedSession("tcp", target, request, token, stats)
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

	cert, err := loadTLSCertificate(address, info)
	if err != nil {
		return nil, err
	}
	return tls.Listen("tcp", address, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"sntp-native/1"},
	})
}

func loadTLSCertificate(address string, info *panel.NodeInfo) (tls.Certificate, error) {
	if info != nil && info.Common != nil && info.Common.CertInfo != nil {
		certInfo := info.Common.CertInfo
		if certInfo.CertFile != "" && certInfo.KeyFile != "" {
			cert, err := tls.LoadX509KeyPair(certInfo.CertFile, certInfo.KeyFile)
			if err == nil {
				return cert, nil
			}
			log.WithFields(log.Fields{
				"listen": address,
				"cert":   certInfo.CertFile,
				"key":    certInfo.KeyFile,
				"err":    err,
			}).Warn("SNTP native TLS cert load failed; using ephemeral self-signed cert")
		}
	}

	log.WithField("listen", address).Warn("SNTP native TLS cert missing; using ephemeral self-signed cert")
	cert, err := generateSelfSignedCertificate()
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate tls cert: %w", err)
	}
	return cert, nil
}

func generateSelfSignedCertificate() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := crand.Int(crand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}

	now := time.Now()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "sntp-native.local",
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"sntp-native.local"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
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

func (i *Ingress) verifyToken(request connectRequest) (*panel.TransportTokenVerifyResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := i.verifier.VerifyTransportToken(ctx, request.AccessToken)
	if err != nil {
		return nil, err
	}
	if result == nil || !result.Valid {
		code := "UNKNOWN"
		if result != nil {
			code = result.Code
		}
		return nil, fmt.Errorf("panel rejected token: %s", code)
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
			return nil, fmt.Errorf("node not accepted by this ingress: %s", result.NodeID)
		}
	}
	return result, nil
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

func dialTCPTarget(host string, port int, timeout time.Duration) (net.Conn, error) {
	if ip := net.ParseIP(host); ip != nil {
		return net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	addrs = preferIPv4(addrs)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for %s", host)
	}

	attemptTimeout := timeout / time.Duration(len(addrs))
	if attemptTimeout < 2*time.Second {
		attemptTimeout = 2 * time.Second
	}
	if attemptTimeout > 5*time.Second {
		attemptTimeout = 5 * time.Second
	}

	var lastErr error
	for _, addr := range addrs {
		target := net.JoinHostPort(addr.IP.String(), strconv.Itoa(port))
		attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptTimeout)
		conn, err := (&net.Dialer{}).DialContext(attemptCtx, "tcp", target)
		attemptCancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("dial %s failed", host)
}

func dialUDPTarget(host string, port int, timeout time.Duration) (*net.UDPConn, error) {
	if ip := net.ParseIP(host); ip != nil {
		return net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	addrs = preferIPv4(addrs)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no address for %s", host)
	}

	var lastErr error
	for _, addr := range addrs {
		conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
			IP:   addr.IP,
			Port: port,
			Zone: addr.Zone,
		})
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("dial udp %s failed", host)
}

func preferIPv4(addrs []net.IPAddr) []net.IPAddr {
	ipv4 := make([]net.IPAddr, 0, len(addrs))
	ipv6 := make([]net.IPAddr, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP.To4() != nil {
			ipv4 = append(ipv4, addr)
		}
	}
	for _, addr := range addrs {
		if addr.IP.To4() == nil {
			ipv6 = append(ipv6, addr)
		}
	}
	rand.Shuffle(len(ipv4), func(i, j int) { ipv4[i], ipv4[j] = ipv4[j], ipv4[i] })
	rand.Shuffle(len(ipv6), func(i, j int) { ipv6[i], ipv6[j] = ipv6[j], ipv6[i] })
	ordered := make([]net.IPAddr, 0, len(addrs))
	ordered = append(ordered, ipv4...)
	ordered = append(ordered, ipv6...)
	return ordered
}

type pipeStats struct {
	clientToTargetBytes int64
	targetToClientBytes int64
	clientToTargetErr   error
	targetToClientErr   error
}

type trafficStats struct {
	upload   int64
	download int64
}

func (i *Ingress) pipeTCP(client net.Conn, clientReader *bufio.Reader, upstream net.Conn, token *panel.TransportTokenVerifyResult) pipeStats {
	var wg sync.WaitGroup
	var stats pipeStats
	wg.Add(2)
	go func() {
		defer wg.Done()
		stats.clientToTargetBytes, stats.clientToTargetErr = i.copyAndRecord(upstream, clientReader, token, true)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		stats.targetToClientBytes, stats.targetToClientErr = i.copyAndRecord(client, upstream, token, false)
		closeWrite(client)
	}()
	wg.Wait()
	gracefulClose(client)
	gracefulClose(upstream)
	stats.clientToTargetErr = cleanPipeError(stats.clientToTargetErr)
	stats.targetToClientErr = cleanPipeError(stats.targetToClientErr)
	return stats
}

func (i *Ingress) copyAndRecord(dst io.Writer, src io.Reader, token *panel.TransportTokenVerifyResult, upload bool) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
				if upload {
					i.recordSessionBytes(token, int64(nw), 0)
				} else {
					i.recordSessionBytes(token, 0, int64(nw))
				}
			}
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

func (i *Ingress) GetUserTrafficSlice(mintraffic int) []panel.UserTraffic {
	i.trafficMu.Lock()
	defer i.trafficMu.Unlock()

	if len(i.trafficByUID) == 0 {
		return nil
	}

	minBytes := int64(mintraffic * 1000)
	trafficSlice := make([]panel.UserTraffic, 0, len(i.trafficByUID))
	for uid, traffic := range i.trafficByUID {
		total := traffic.upload + traffic.download
		if total <= minBytes {
			continue
		}
		trafficSlice = append(trafficSlice, panel.UserTraffic{
			UID:      uid,
			Upload:   traffic.upload,
			Download: traffic.download,
		})
		delete(i.trafficByUID, uid)
	}
	if len(trafficSlice) == 0 {
		return nil
	}
	return trafficSlice
}

func (i *Ingress) recordTraffic(uid int, upload int64, download int64) {
	if uid <= 0 || upload+download <= 0 {
		return
	}
	i.trafficMu.Lock()
	defer i.trafficMu.Unlock()
	traffic := i.trafficByUID[uid]
	if traffic == nil {
		traffic = &trafficStats{}
		i.trafficByUID[uid] = traffic
	}
	traffic.upload += upload
	traffic.download += download
}

func prepareTCPConn(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(30 * time.Second)
	}
}

func closeWrite(conn net.Conn) {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
		return
	}
	_ = conn.Close()
}

func gracefulClose(conn net.Conn) {
	_ = conn.Close()
}

func cleanPipeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
		return nil
	}
	return err
}

func (i *Ingress) handleUDPSession(conn net.Conn, reader *bufio.Reader, request connectRequest, token *panel.TransportTokenVerifyResult) {
	target := net.JoinHostPort(request.Host, strconv.Itoa(request.Port))
	upstream, err := dialUDPTarget(request.Host, request.Port, 10*time.Second)
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
	i.logAcceptedSession("udp", target, upstream.RemoteAddr().String(), request, token)
	stats := i.pipeUDP(conn, reader, upstream, token)
	i.logFinishedSession("udp", target, request, token, stats)
}

func (i *Ingress) logAcceptedSession(network string, target string, upstream string, request connectRequest, token *panel.TransportTokenVerifyResult) {
	fields := log.Fields{
		"tag":          i.tag,
		"network":      network,
		"target":       target,
		"upstream":     upstream,
		"request_node": request.NodeID,
	}
	if token != nil {
		fields["auth_node"] = token.NodeID
		fields["uid"] = token.UID
		fields["device_uuid"] = token.DeviceUUID
	}
	log.WithFields(fields).Info("SNTP native session accepted")
}

func (i *Ingress) logFinishedSession(network string, target string, request connectRequest, token *panel.TransportTokenVerifyResult, stats pipeStats) {
	fields := log.Fields{
		"tag":                    i.tag,
		"network":                network,
		"target":                 target,
		"request_node":           request.NodeID,
		"client_to_target_bytes": stats.clientToTargetBytes,
		"target_to_client_bytes": stats.targetToClientBytes,
	}
	if token != nil {
		fields["auth_node"] = token.NodeID
		fields["uid"] = token.UID
		fields["device_uuid"] = token.DeviceUUID
	}
	if stats.clientToTargetErr != nil {
		fields["client_to_target_err"] = stats.clientToTargetErr.Error()
	}
	if stats.targetToClientErr != nil {
		fields["target_to_client_err"] = stats.targetToClientErr.Error()
	}
	log.WithFields(fields).Info("SNTP native session finished")
}

func (i *Ingress) recordSessionBytes(token *panel.TransportTokenVerifyResult, upload int64, download int64) {
	if token == nil {
		return
	}
	i.recordTraffic(token.UID, upload, download)
}

func (i *Ingress) pipeUDP(client net.Conn, clientReader *bufio.Reader, upstream *net.UDPConn, token *panel.TransportTokenVerifyResult) pipeStats {
	var writeMu sync.Mutex
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}

	var wg sync.WaitGroup
	var stats pipeStats
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			payload, err := readFrame(clientReader)
			if err != nil {
				stats.clientToTargetErr = err
				closeBoth()
				return
			}
			stats.clientToTargetBytes += int64(len(payload))
			i.recordSessionBytes(token, int64(len(payload)), 0)
			if _, err := upstream.Write(payload); err != nil {
				stats.clientToTargetErr = err
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
				stats.targetToClientErr = err
				closeBoth()
				return
			}
			stats.targetToClientBytes += int64(n)
			i.recordSessionBytes(token, 0, int64(n))
			writeMu.Lock()
			err = writeFrame(client, buf[:n])
			writeMu.Unlock()
			if err != nil {
				stats.targetToClientErr = err
				closeBoth()
				return
			}
		}
	}()
	wg.Wait()
	return stats
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
