package native

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	panel "github.com/wyx2685/v2node/api/v2board"
)

type Ingress struct {
	tag     string
	tcp     net.Listener
	udp     *net.UDPConn
	done    chan struct{}
	closeMu sync.Once
	wg      sync.WaitGroup
}

func Start(tag string, info *panel.NodeInfo) (*Ingress, error) {
	if info == nil || info.Common == nil {
		return nil, fmt.Errorf("empty sntp-native node info")
	}

	listenIP := info.Common.ListenIP
	if listenIP == "" {
		listenIP = "0.0.0.0"
	}
	address := net.JoinHostPort(listenIP, strconv.Itoa(info.Common.ServerPort))
	tcp, err := net.Listen("tcp", address)
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
		tag:  tag,
		tcp:  tcp,
		udp:  udp,
		done: make(chan struct{}),
	}
	ingress.wg.Add(2)
	go ingress.acceptTCP()
	go ingress.readUDP()
	log.WithFields(log.Fields{"tag": tag, "listen": address}).Info("SNTP native ingress skeleton started")
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
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte("SNTP_NATIVE_V1 ERROR NOT_IMPLEMENTED\n"))
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
