// The WebSocket handshake/listener is adapted from Xray-core's
// transport/internet/websocket/hub.go (MPL-2.0).
// Source: https://github.com/wyx2685/xray-core/tree/dcfae7c4776b/transport/internet/websocket
// SNTP changes: verify the Cloudflare socket peer before resolving client IP.
package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet"
	v2tls "github.com/xtls/xray-core/transport/internet/tls"
	xws "github.com/xtls/xray-core/transport/internet/websocket"
	"google.golang.org/protobuf/proto"
)

const cloudflareWebSocket = "sntp-cf-websocket"

// Private transport metadata, never sent as a request or response header.
const cloudflareCIDRMetadata = "X-SNTP-Internal-CF-CIDRs"

func enableCloudflareWebSocket(in *xcore.InboundHandlerConfig, policy *panel.CloudflareOriginConfig) error {
	raw, err := in.ReceiverSettings.GetInstance()
	if err != nil {
		return err
	}
	receiver, ok := raw.(*proxyman.ReceiverConfig)
	if !ok || receiver.StreamSettings == nil {
		return nil
	}
	stream := receiver.StreamSettings
	if stream.ProtocolName != "websocket" || stream.SocketSettings.GetAcceptProxyProtocol() {
		return nil
	}
	// Masked listeners may replace RemoteAddr; only an actual socket is trusted.
	if len(stream.Tcpmasks) > 0 || len(stream.Udpmasks) > 0 {
		return nil
	}
	effective, err := stream.GetEffectiveTransportSettings()
	if err != nil {
		return err
	}
	ws, ok := effective.(*xws.Config)
	if !ok {
		return errors.New("invalid WebSocket transport config")
	}
	if ws.AcceptProxyProtocol {
		return nil
	}
	ws = proto.Clone(ws).(*xws.Config)
	for key := range ws.Header {
		if strings.EqualFold(key, cloudflareCIDRMetadata) {
			delete(ws.Header, key)
		}
	}
	if policy != nil && policy.Managed {
		prefixes, err := validatedCloudflarePrefixes(policy.CIDRs)
		if err != nil {
			return fmt.Errorf("invalid managed Cloudflare origin policy: %w", err)
		}
		cidrs := make([]string, len(prefixes))
		for i, prefix := range prefixes {
			cidrs[i] = prefix.String()
		}
		encoded, err := json.Marshal(cidrs)
		if err != nil {
			return err
		}
		if ws.Header == nil {
			ws.Header = make(map[string]string)
		}
		ws.Header[cloudflareCIDRMetadata] = string(encoded)
	}
	found := false
	for _, setting := range stream.TransportSettings {
		if setting.ProtocolName != "websocket" {
			continue
		}
		setting.ProtocolName = cloudflareWebSocket
		setting.Settings = serial.ToTypedMessage(ws)
		found = true
		break
	}
	if !found {
		stream.TransportSettings = append(stream.TransportSettings, &internet.TransportConfig{
			ProtocolName: cloudflareWebSocket,
			Settings:     serial.ToTypedMessage(ws),
		})
	}
	stream.ProtocolName = cloudflareWebSocket
	in.ReceiverSettings = serial.ToTypedMessage(receiver)
	return nil
}

type cloudflareWSHandler struct {
	config  *xws.Config
	peers   *cloudflareRangeStore
	addConn internet.ConnHandler
}

func (h *cloudflareWSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if (h.config.Host != "" && !internet.IsValidHTTPHost(r.Host, h.config.Host)) || r.URL.Path != h.config.GetNormalizedPath() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	var extra io.Reader
	response := http.Header{}
	if protocol := r.Header.Get("Sec-WebSocket-Protocol"); protocol != "" {
		encoded := strings.NewReplacer("+", "-", "/", "_", "=", "").Replace(protocol)
		if data, err := base64.RawURLEncoding.DecodeString(encoded); err == nil && len(data) > 0 {
			extra = bytes.NewReader(data)
			response.Set("Sec-WebSocket-Protocol", protocol)
		}
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 4 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, response)
	if err != nil {
		return
	}
	peer := h.peers.remoteAddr(conn.RemoteAddr(), r.Header)
	h.addConn(xws.NewConnection(conn, peer, extra, h.config.HeartbeatPeriod))
}

type cloudflareWSListener struct{ net.Listener }

func listenCloudflareWS(ctx context.Context, address xnet.Address, port xnet.Port, settings *internet.MemoryStreamConfig, addConn internet.ConnHandler) (internet.Listener, error) {
	config, ok := settings.ProtocolSettings.(*xws.Config)
	if !ok || config == nil {
		return nil, errors.New("invalid Cloudflare WebSocket config")
	}
	if config.AcceptProxyProtocol || settings.SocketSettings.GetAcceptProxyProtocol() || settings.FinalMask != nil {
		return nil, errors.New("Cloudflare WebSocket requires the original socket peer")
	}
	peers, err := cloudflareStoreForWebSocket(config)
	if err != nil {
		return nil, err
	}
	var addr net.Addr = &net.TCPAddr{IP: address.IP(), Port: int(port)}
	if port == 0 {
		addr = &net.UnixAddr{Name: address.Domain(), Net: "unix"}
	}
	listener, err := internet.ListenSystem(ctx, addr, settings.SocketSettings)
	if err != nil {
		return nil, fmt.Errorf("listen Cloudflare WebSocket: %w", err)
	}
	if config := v2tls.ConfigFromStreamSettings(settings); config != nil {
		if tlsConfig := config.GetTLSConfig(); tlsConfig != nil {
			listener = tls.NewListener(listener, tlsConfig)
		}
	}
	server := &http.Server{
		Handler:           &cloudflareWSHandler{config: config, peers: peers, addConn: addConn},
		ReadHeaderTimeout: 4 * time.Second, MaxHeaderBytes: 8192,
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("Cloudflare WebSocket HTTP listener stopped unexpectedly")
		}
	}()
	// Like the upstream listener, closing the accept socket preserves hijacked connections.
	return &cloudflareWSListener{Listener: listener}, nil
}

func cloudflareStoreForWebSocket(config *xws.Config) (*cloudflareRangeStore, error) {
	encoded, managed := config.Header[cloudflareCIDRMetadata]
	if !managed {
		return cloudflarePeers, nil
	}
	if len(encoded) > 16384 {
		return nil, errors.New("managed Cloudflare CIDRs too large")
	}
	var cidrs []string
	if err := json.Unmarshal([]byte(encoded), &cidrs); err != nil {
		return nil, errors.New("invalid managed Cloudflare CIDRs")
	}
	prefixes, err := validatedCloudflarePrefixes(cidrs)
	if err != nil {
		return nil, err
	}
	store := new(cloudflareRangeStore)
	store.prefixes.Store(&prefixes)
	return store, nil
}

func init() {
	common.Must(internet.RegisterProtocolConfigCreator(cloudflareWebSocket, func() interface{} { return new(xws.Config) }))
	common.Must(internet.RegisterTransportListener(cloudflareWebSocket, listenCloudflareWS))
}
