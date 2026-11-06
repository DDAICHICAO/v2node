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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/common"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/serial"
	xcore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet"
	v2tls "github.com/xtls/xray-core/transport/internet/tls"
	xws "github.com/xtls/xray-core/transport/internet/websocket"
)

const cloudflareWebSocket = "sntp-cf-websocket"

func enableCloudflareWebSocket(in *xcore.InboundHandlerConfig) error {
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
	found := false
	for _, setting := range stream.TransportSettings {
		if setting.ProtocolName != "websocket" {
			continue
		}
		setting.ProtocolName = cloudflareWebSocket
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
		Handler:           &cloudflareWSHandler{config: config, peers: cloudflarePeers, addConn: addConn},
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

func init() {
	common.Must(internet.RegisterProtocolConfigCreator(cloudflareWebSocket, func() interface{} { return new(xws.Config) }))
	common.Must(internet.RegisterTransportListener(cloudflareWebSocket, listenCloudflareWS))
}
