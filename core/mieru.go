package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	mierucommon "github.com/enfein/mieru/v3/apis/common"
	mieruconstant "github.com/enfein/mieru/v3/apis/constant"
	mierumodel "github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	mierupb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/wyx2685/v2node/common/counter"
	"github.com/wyx2685/v2node/common/format"
	"github.com/wyx2685/v2node/limiter"
	xbuf "github.com/xtls/xray-core/common/buf"
	xlog "github.com/xtls/xray-core/common/log"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	udp_proto "github.com/xtls/xray-core/common/protocol/udp"
	"github.com/xtls/xray-core/common/session"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	xudp "github.com/xtls/xray-core/transport/internet/udp"
	"google.golang.org/protobuf/proto"
)

const (
	mieruProtocol      = "mieru"
	mieruNetworkTCPUDP = "tcp_udp"
	mieruTransportTCP  = "TCP"
	mieruTransportUDP  = "UDP"
)

type MieruServer struct {
	tag        string
	nodeID     int
	listenIP   string
	serverPort int
	transports []string

	xray       *xcore.Instance
	dispatcher routing.Dispatcher
	counter    *counter.TrafficCounter

	mu      sync.Mutex
	server  mieruserver.Server
	running bool
	closed  bool
	users   map[string]mieruUser
}

type mieruUser struct {
	UID  int
	UUID string
}

type mieruNetworkSettings struct {
	Transport  string   `json:"transport"`
	Transports []string `json:"transports"`
}

type mieruListenerFactory struct {
	listenIP string
}

type mieruSession struct {
	conn       net.Conn
	clientIP   string
	userTag    string
	uid        int
	tag        string
	xray       *xcore.Instance
	dispatcher routing.Dispatcher
	writeMu    sync.Mutex
}

func newMieruServer(tag string, nodeInfo *panel.NodeInfo, xray *xcore.Instance, dispatcher routing.Dispatcher) (*MieruServer, error) {
	if nodeInfo == nil || nodeInfo.Common == nil {
		return nil, errors.New("missing node info")
	}
	if xray == nil {
		return nil, errors.New("missing xray instance")
	}
	if dispatcher == nil {
		return nil, errors.New("missing routing dispatcher")
	}
	transports, err := mieruTransports(nodeInfo)
	if err != nil {
		return nil, err
	}
	listenIP := strings.TrimSpace(nodeInfo.Common.ListenIP)
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	return &MieruServer{
		tag:        tag,
		nodeID:     nodeInfo.Id,
		listenIP:   listenIP,
		serverPort: nodeInfo.Common.ServerPort,
		transports: transports,
		xray:       xray,
		dispatcher: dispatcher,
		counter:    counter.NewTrafficCounter(),
		users:      make(map[string]mieruUser),
	}, nil
}

func (s *MieruServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked()
}

func (s *MieruServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.running = false
	if s.server == nil {
		return nil
	}
	err := s.server.Stop()
	s.server = nil
	return err
}

func (s *MieruServer) SetUsers(users []panel.UserInfo) error {
	next := make(map[string]mieruUser, len(users))
	for _, user := range users {
		next[mieruUsername(user)] = mieruUser{UID: user.Id, UUID: user.Uuid}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = next
	return s.restartLocked()
}

func (s *MieruServer) AddUsers(users []panel.UserInfo) error {
	if len(users) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range users {
		s.users[mieruUsername(user)] = mieruUser{UID: user.Id, UUID: user.Uuid}
	}
	if !s.running {
		return s.startLocked()
	}
	return s.restartLocked()
}

func (s *MieruServer) DelUsers(users []panel.UserInfo) error {
	if len(users) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range users {
		delete(s.users, mieruUsername(user))
		s.counter.Delete(format.UserTag(s.tag, user.Uuid))
	}
	return s.restartLocked()
}

func (s *MieruServer) lookupUser(username string) (mieruUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[username]
	return user, ok
}

func (s *MieruServer) startLocked() error {
	if s.closed {
		return errors.New("mieru server is closed")
	}
	if s.running {
		return nil
	}
	if len(s.users) == 0 {
		return errors.New("missing mieru users")
	}
	server, err := s.newAPIServerLocked()
	if err != nil {
		return err
	}
	if err := server.Start(); err != nil {
		return err
	}
	s.server = server
	s.running = true
	go s.acceptLoop(server)
	log.WithFields(log.Fields{
		"tag":        s.tag,
		"listen":     net.JoinHostPort(s.listenIP, strconv.Itoa(s.serverPort)),
		"transports": strings.Join(s.transports, ","),
	}).Info("Mieru ingress started")
	return nil
}

func (s *MieruServer) restartLocked() error {
	if s.closed {
		return nil
	}
	if s.server != nil {
		_ = s.server.Stop()
		s.server = nil
		s.running = false
	}
	if len(s.users) == 0 {
		return nil
	}
	return s.startLocked()
}

func (s *MieruServer) newAPIServerLocked() (mieruserver.Server, error) {
	portBindings := make([]*mierupb.PortBinding, 0, len(s.transports))
	for _, transport := range s.transports {
		protocolValue := mierupb.TransportProtocol_TCP
		if transport == mieruTransportUDP {
			protocolValue = mierupb.TransportProtocol_UDP
		}
		portBindings = append(portBindings, &mierupb.PortBinding{
			Port:     proto.Int32(int32(s.serverPort)),
			Protocol: protocolValue.Enum(),
		})
	}

	usernames := make([]string, 0, len(s.users))
	for username := range s.users {
		usernames = append(usernames, username)
	}
	sort.Strings(usernames)
	users := make([]*mierupb.User, 0, len(usernames))
	for _, username := range usernames {
		user := s.users[username]
		users = append(users, &mierupb.User{
			Name:     proto.String(username),
			Password: proto.String(user.UUID),
		})
	}

	server := mieruserver.NewServer()
	listenerFactory := mieruListenerFactory{listenIP: s.listenIP}
	config := &mieruserver.ServerConfig{
		Config: &mierupb.ServerConfig{
			PortBindings: portBindings,
			Users:        users,
		},
		StreamListenerFactory: listenerFactory,
		PacketListenerFactory: listenerFactory,
	}
	if err := server.Store(config); err != nil {
		return nil, fmt.Errorf("store mieru config: %w", err)
	}
	return server, nil
}

func (s *MieruServer) acceptLoop(server mieruserver.Server) {
	for {
		conn, req, err := server.Accept()
		if err != nil {
			if !server.IsRunning() {
				return
			}
			log.WithFields(log.Fields{"tag": s.tag, "err": err}).Warn("Mieru accept failed")
			continue
		}
		go s.handleConn(conn, req)
	}
}

func (s *MieruServer) handleConn(conn net.Conn, req *mierumodel.Request) {
	clientIP := remoteIP(conn.RemoteAddr())
	username := ""
	if userCtx, ok := conn.(mierucommon.UserContext); ok {
		username = userCtx.UserName()
	}
	user, ok := s.lookupUser(username)
	if !ok {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"username":  username,
			"reason":    limiter.LimitRejectReasonUserNotFound.String(),
		}).Warn("Mieru user not found")
		_ = writeMieruSocksResponse(conn, mieruconstant.Socks5ReplyNotAllowedByRuleSet)
		_ = conn.Close()
		return
	}

	userTag := format.UserTag(s.tag, user.UUID)
	isTCP := req != nil && req.Command == mieruconstant.Socks5ConnectCmd
	if l, err := limiter.GetLimiter(s.tag); err == nil {
		if _, reject, rejectInfo := l.CheckLimit(userTag, clientIP, isTCP); reject {
			log.WithFields(log.Fields{
				"tag":                    s.tag,
				"client_ip":              clientIP,
				"uid":                    user.UID,
				"target":                 reqTarget(req),
				"cmd":                    reqCommand(req),
				"reason":                 rejectInfo.Reason.String(),
				"device_limit":           rejectInfo.DeviceLimit,
				"alive_count":            rejectInfo.AliveCount,
				"pending_device_count":   rejectInfo.PendingDeviceCount,
				"cached_device_overlap":  rejectInfo.CachedDeviceOverlap,
				"effective_device_count": rejectInfo.EffectiveDeviceCount,
				"device_limit_by_uuid":   rejectInfo.UseDeviceLimitByUUID,
			}).Warn("Mieru user rejected by limiter")
			_ = writeMieruSocksResponse(conn, mieruconstant.Socks5ReplyNotAllowedByRuleSet)
			_ = conn.Close()
			return
		}
	}

	if err := writeMieruSocksResponse(conn, mieruconstant.Socks5ReplySuccess); err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uid":       user.UID,
			"target":    reqTarget(req),
			"err":       err,
		}).Warn("Mieru reply failed")
		_ = conn.Close()
		return
	}

	session := &mieruSession{
		conn:       conn,
		clientIP:   clientIP,
		userTag:    userTag,
		uid:        user.UID,
		tag:        s.tag,
		xray:       s.xray,
		dispatcher: s.dispatcher,
	}
	switch reqCommand(req) {
	case mieruconstant.Socks5ConnectCmd:
		session.serveTCP(req)
	case mieruconstant.Socks5UDPAssociateCmd:
		session.serveUDP()
	default:
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"client_ip": clientIP,
			"uid":       user.UID,
			"cmd":       reqCommand(req),
			"target":    reqTarget(req),
		}).Warn("Mieru unsupported command")
		_ = conn.Close()
	}
}

func (s *mieruSession) serveTCP(req *mierumodel.Request) {
	defer s.conn.Close()
	destination, err := mieruDestination(xnet.Network_TCP, req.DstAddr)
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"uid":       s.uid,
			"client_ip": s.clientIP,
			"err":       err,
		}).Warn("Mieru TCP destination invalid")
		return
	}
	ctx := s.dispatchContext(destination)
	ctx = xlog.ContextWithAccessMessage(ctx, &xlog.AccessMessage{
		From:   s.dispatchSource(xnet.Network_TCP),
		To:     destination,
		Status: xlog.AccessAccepted,
		Email:  s.userTag,
	})
	log.WithFields(log.Fields{
		"tag":       s.tag,
		"uid":       s.uid,
		"target":    destination.NetAddr(),
		"client_ip": s.clientIP,
	}).Info("Mieru TCP session accepted")

	err = s.dispatcher.DispatchLink(ctx, destination, &transport.Link{
		Reader: xbuf.NewReader(s.conn),
		Writer: xbuf.NewWriter(s.conn),
	})
	if err != nil {
		log.WithFields(log.Fields{
			"tag":       s.tag,
			"uid":       s.uid,
			"target":    destination.NetAddr(),
			"client_ip": s.clientIP,
			"err":       err,
		}).Warn("Mieru TCP dispatch failed")
	}
}

func (s *mieruSession) serveUDP() {
	ctx, cancel := context.WithCancel(s.dispatchContext(xnet.UDPDestination(xnet.AnyIP, 0)))
	defer cancel()
	defer s.conn.Close()

	packetConn := mierucommon.NewPacketOverStreamTunnel(s.conn)
	udpServer := xudp.NewDispatcher(s.dispatcher, func(ctx context.Context, packet *udp_proto.Packet) {
		payload := packet.Payload
		defer payload.Release()
		source := packet.Source
		if payload.UDP != nil {
			source = *payload.UDP
		}
		if payload.IsEmpty() {
			return
		}
		data, err := encodeMieruUDPPacket(source, payload.Bytes())
		if err != nil {
			log.WithFields(log.Fields{
				"tag":       s.tag,
				"uid":       s.uid,
				"target":    source.NetAddr(),
				"client_ip": s.clientIP,
				"err":       err,
			}).Warn("Mieru UDP encode reply failed")
			cancel()
			return
		}
		s.writeMu.Lock()
		_, err = packetConn.WriteTo(data, nil)
		s.writeMu.Unlock()
		if err != nil {
			log.WithFields(log.Fields{
				"tag":       s.tag,
				"uid":       s.uid,
				"target":    source.NetAddr(),
				"client_ip": s.clientIP,
				"err":       err,
			}).Warn("Mieru UDP reply failed")
			cancel()
		}
	})
	defer udpServer.RemoveRay()

	buf := make([]byte, 65535)
	for {
		n, _, err := packetConn.ReadFrom(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.ErrClosedPipe) {
				log.WithFields(log.Fields{
					"tag":       s.tag,
					"uid":       s.uid,
					"client_ip": s.clientIP,
					"err":       err,
				}).Debug("Mieru UDP read failed")
			}
			return
		}
		destination, payload, err := decodeMieruUDPPacket(buf[:n])
		if err != nil || len(payload) == 0 {
			continue
		}
		currentCtx := xlog.ContextWithAccessMessage(ctx, &xlog.AccessMessage{
			From:   s.dispatchSource(xnet.Network_UDP),
			To:     destination,
			Status: xlog.AccessAccepted,
			Email:  s.userTag,
		})
		mb := xbuf.NewWithSize(int32(len(payload)))
		copy(mb.Extend(int32(len(payload))), payload)
		mb.UDP = &destination
		udpServer.Dispatch(currentCtx, destination, mb)
	}
}

func (s *mieruSession) dispatchContext(destination xnet.Destination) context.Context {
	source := s.dispatchSource(destination.Network)
	baseCtx := context.Background()
	if s.xray != nil {
		baseCtx = context.WithValue(baseCtx, xcore.XrayKey(1), s.xray)
	}
	ctx := session.ContextWithInbound(baseCtx, &session.Inbound{
		Source: source,
		Tag:    s.tag,
		Name:   mieruProtocol,
		User: &protocol.MemoryUser{
			Level: 0,
			Email: s.userTag,
		},
		CanSpliceCopy: 3,
	})
	ctx = session.ContextWithDispatcher(ctx, s.dispatcher)
	return session.ContextWithContent(ctx, &session.Content{
		SniffingRequest: session.SniffingRequest{
			Enabled: true,
			OverrideDestinationForProtocol: []string{
				"http",
				"tls",
				"quic",
			},
		},
	})
}

func (s *mieruSession) dispatchSource(network xnet.Network) xnet.Destination {
	address := xnet.ParseAddress(strings.TrimSpace(s.clientIP))
	if network == xnet.Network_UDP {
		return xnet.UDPDestination(address, 0)
	}
	return xnet.TCPDestination(address, 0)
}

func (f mieruListenerFactory) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	var listenConfig net.ListenConfig
	return listenConfig.Listen(ctx, network, f.bindAddress(address))
}

func (f mieruListenerFactory) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	var listenConfig net.ListenConfig
	return listenConfig.ListenPacket(ctx, network, f.bindAddress(address))
}

func (f mieruListenerFactory) bindAddress(address string) string {
	listenIP := strings.TrimSpace(f.listenIP)
	if listenIP == "" {
		return address
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return address
	}
	return net.JoinHostPort(listenIP, port)
}

func writeMieruSocksResponse(conn net.Conn, reply byte) error {
	resp := &mierumodel.Response{
		Reply: reply,
		BindAddr: mierumodel.AddrSpec{
			IP:   net.IPv4zero,
			Port: 0,
		},
	}
	return resp.WriteToSocks5(conn)
}

func decodeMieruUDPPacket(packet []byte) (xnet.Destination, []byte, error) {
	if len(packet) < 4 {
		return xnet.Destination{}, nil, errors.New("short udp packet")
	}
	if packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return xnet.Destination{}, nil, errors.New("unsupported udp fragment")
	}
	reader := bytes.NewReader(packet[3:])
	addr := mierumodel.AddrSpec{}
	if err := addr.ReadFromSocks5(reader); err != nil {
		return xnet.Destination{}, nil, err
	}
	offset := len(packet) - reader.Len()
	destination, err := mieruDestination(xnet.Network_UDP, addr)
	if err != nil {
		return xnet.Destination{}, nil, err
	}
	return destination, packet[offset:], nil
}

func encodeMieruUDPPacket(destination xnet.Destination, payload []byte) ([]byte, error) {
	addr, err := mieruAddrSpecFromDestination(destination)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write([]byte{0, 0, 0})
	if err := addr.WriteToSocks5(&buf); err != nil {
		return nil, err
	}
	buf.Write(payload)
	return buf.Bytes(), nil
}

func mieruDestination(network xnet.Network, addr mierumodel.AddrSpec) (xnet.Destination, error) {
	if addr.Port <= 0 || addr.Port > 65535 {
		return xnet.Destination{}, fmt.Errorf("invalid port: %d", addr.Port)
	}
	host := strings.TrimSpace(addr.FQDN)
	if host == "" && len(addr.IP) > 0 {
		host = addr.IP.String()
	}
	if host == "" {
		return xnet.Destination{}, errors.New("missing host")
	}
	address := xnet.ParseAddress(host)
	port := xnet.Port(addr.Port)
	if network == xnet.Network_UDP {
		return xnet.UDPDestination(address, port), nil
	}
	return xnet.TCPDestination(address, port), nil
}

func mieruAddrSpecFromDestination(destination xnet.Destination) (mierumodel.AddrSpec, error) {
	addr := mierumodel.AddrSpec{Port: int(destination.Port)}
	switch destination.Address.Family() {
	case xnet.AddressFamilyDomain:
		addr.FQDN = destination.Address.Domain()
	case xnet.AddressFamilyIPv4, xnet.AddressFamilyIPv6:
		addr.IP = destination.Address.IP()
	default:
		return addr, errors.New("unsupported address family")
	}
	return addr, nil
}

func mieruTransports(nodeInfo *panel.NodeInfo) ([]string, error) {
	transports := make([]string, 0, 2)

	if nodeInfo != nil && nodeInfo.Common != nil {
		network := strings.ToLower(strings.TrimSpace(nodeInfo.Common.Network))
		switch network {
		case mieruNetworkTCPUDP:
			return []string{mieruTransportTCP, mieruTransportUDP}, nil
		case "tcp":
			return []string{mieruTransportTCP}, nil
		case "udp":
			return []string{mieruTransportUDP}, nil
		case "":
			if len(nodeInfo.Common.NetworkSettings) > 0 {
				settings := &mieruNetworkSettings{}
				if err := json.Unmarshal(nodeInfo.Common.NetworkSettings, settings); err != nil {
					return nil, fmt.Errorf("unmarshal mieru network settings error: %w", err)
				}
				for _, value := range settings.Transports {
					transports = appendMieruTransport(transports, value)
				}
				transports = appendMieruTransport(transports, settings.Transport)
			}
		default:
			return nil, fmt.Errorf("unsupported mieru network: %s", nodeInfo.Common.Network)
		}
	}

	if len(transports) == 0 {
		transports = append(transports, mieruTransportTCP, mieruTransportUDP)
	}
	return transports, nil
}

func appendMieruTransport(transports []string, value string) []string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case mieruTransportTCP, mieruTransportUDP:
		for _, existing := range transports {
			if existing == value {
				return transports
			}
		}
		return append(transports, value)
	default:
		return transports
	}
}

func mieruUsername(user panel.UserInfo) string {
	if user.Id > 0 {
		return strconv.Itoa(user.Id)
	}
	return strings.TrimSpace(user.Uuid)
}

func reqCommand(req *mierumodel.Request) byte {
	if req == nil {
		return 0
	}
	return req.Command
}

func reqTarget(req *mierumodel.Request) string {
	if req == nil {
		return ""
	}
	return req.DstAddr.String()
}

func isMieruNode(info *panel.NodeInfo) bool {
	return info != nil && info.Type == mieruProtocol
}

func isMieruProtocol(protocol string) bool {
	return strings.EqualFold(strings.TrimSpace(protocol), mieruProtocol)
}
