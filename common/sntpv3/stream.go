// Package sntpv3 carries an existing authenticated cipher stream across WSS
// reconnects. Both SNTP endpoints use the same versioned wire implementation.
package sntpv3

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const MaxPayload = 16384
const headerSize = 38
const (
	dataPacket  byte = 1
	ackPacket   byte = 2
	finPacket   byte = 3
	closePacket byte = 4
)

type Options struct {
	SendBudget, ReceiveBudget *Budget
	ClockOffset               time.Duration
	Window                    time.Duration
	BufferBytes               int
	Heartbeat                 time.Duration
	FailureTimeout            time.Duration
}

func (o Options) normalized() Options {
	if o.Window <= 0 {
		o.Window = 30 * time.Second
	}
	if o.Window > 120*time.Second {
		o.Window = 120 * time.Second
	}
	if o.BufferBytes < MaxPayload {
		o.BufferBytes = 1 << 20
	}
	if o.BufferBytes > 4<<20 {
		o.BufferBytes = 4 << 20
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = time.Second
	}
	if o.FailureTimeout <= 0 {
		o.FailureTimeout = 2500 * time.Millisecond
	}
	return o
}

type ResumeFunc func(context.Context, *Conn) (net.Conn, uint64, uint64, uint64, error)

// Conn owns cipher bytes, not plaintext or destination connections. A failed
// wire cannot close this connection before the bounded recovery window expires.
type Conn struct {
	rxReserved                                      int
	mu                                              sync.Mutex
	readMu, writeMu, wireMu                         sync.Mutex
	id                                              [16]byte
	key                                             [32]byte
	side                                            byte
	opts                                            Options
	wire                                            net.Conn
	ready                                           bool
	epoch, proposal                                 uint64
	txNext, txAck, txSent, peerRead, rxNext, rxRead uint64
	txBuf, rxBuf                                    []byte
	txFIN, rxFIN, finAck                            bool
	err                                             error
	changed                                         chan struct{}
	kick                                            chan struct{}
	done                                            chan struct{}
	detached                                        time.Time
	rd, wd                                          time.Time
	local, remote                                   net.Addr
	resume                                          ResumeFunc
	onDetach                                        func(net.Addr)
	onClose                                         func()
}

func New(id [16]byte, key [32]byte, side byte, opts Options) *Conn {
	c := &Conn{id: id, key: key, side: side, opts: opts.normalized(), changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}), detached: time.Now()}
	go c.run()
	return c
}
func (c *Conn) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
	select {
	case c.kick <- struct{}{}:
	default:
	}
}
func (c *Conn) ID() [16]byte          { return c.id }
func (c *Conn) Done() <-chan struct{} { return c.done }
func (c *Conn) NextEpoch() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.proposal < c.epoch {
		c.proposal = c.epoch
	}
	c.proposal++
	return c.proposal
}
func (c *Conn) Progress() (uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rxNext, c.rxRead
}
func (c *Conn) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *Conn) SetResume(f ResumeFunc) {
	c.mu.Lock()
	c.resume = f
	c.notify()
	c.mu.Unlock()
}
func (c *Conn) SetOnDetach(f func(net.Addr)) { c.mu.Lock(); c.onDetach = f; c.mu.Unlock() }
func (c *Conn) SetOnClose(f func()) {
	c.mu.Lock()
	closed := c.err != nil
	if !closed {
		c.onClose = f
	}
	c.mu.Unlock()
	if closed && f != nil {
		f()
	}
}

func (c *Conn) checkPeer(ack, read uint64) error {
	if ack < c.txAck || ack > c.txNext || read < c.peerRead || read > ack {
		return errors.New("invalid V3 peer progress")
	}
	return nil
}
func (c *Conn) advancePeer(ack, read uint64) {
	trim := int(ack - c.txAck)
	c.opts.SendBudget.release(trim)
	c.txBuf = c.txBuf[trim:]
	if len(c.txBuf) == 0 {
		c.txBuf = nil
	}
	c.txAck = ack
	if c.txSent < ack {
		c.txSent = ack
	}
	c.peerRead = read
}
func (c *Conn) Attach(w net.Conn, epoch, ack, read uint64) error {
	return c.attach(w, epoch, ack, read, nil)
}

// The authenticated server reply and epoch change occur while the wire writer
// is fenced. Old read goroutines can no longer mutate the session afterwards.
func (c *Conn) attach(w net.Conn, epoch, ack, read uint64, reply func(uint64, uint64) error) error {
	c.wireMu.Lock()
	c.mu.Lock()
	if c.err != nil || epoch == 0 || epoch <= c.epoch {
		c.mu.Unlock()
		c.wireMu.Unlock()
		return errors.New("closed or superseded V3 session")
	}
	if err := c.checkPeer(ack, read); err != nil {
		c.mu.Unlock()
		c.wireMu.Unlock()
		return err
	}
	old := c.wire
	c.wire = w
	c.ready = false
	c.epoch = epoch
	c.txSent = ack
	c.advancePeer(ack, read)
	if c.detached.IsZero() {
		c.detached = time.Now()
	}
	c.local, c.remote = w.LocalAddr(), w.RemoteAddr()
	rx, consumed := c.rxNext, c.rxRead
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	var err error
	if reply != nil {
		err = reply(rx, consumed)
	}
	c.mu.Lock()
	if err == nil && c.err == nil && c.wire == w && c.epoch == epoch {
		c.ready = true
		c.detached = time.Time{}
		c.notify()
	} else if err == nil {
		err = net.ErrClosed
	}
	c.mu.Unlock()
	c.wireMu.Unlock()
	if err != nil {
		c.detach(w, epoch)
		return err
	}
	go c.receive(w, epoch)
	return nil
}

func (c *Conn) detach(w net.Conn, epoch uint64) {
	c.mu.Lock()
	if c.err != nil || c.wire != w || c.epoch != epoch {
		c.mu.Unlock()
		return
	}
	addr := c.remote
	c.wire = nil
	c.ready = false
	if c.detached.IsZero() {
		c.detached = time.Now()
	}
	c.notify()
	hook := c.onDetach
	c.mu.Unlock()
	_ = w.Close()
	if hook != nil {
		hook(addr)
	}
}
func (c *Conn) finish(err error, sendClose bool) error {
	return c.finishIf(err, sendClose, nil)
}
func (c *Conn) finishIf(err error, sendClose bool, valid func() bool) error {
	if sendClose {
		_ = c.send(closePacket)
	}
	c.mu.Lock()
	if c.err != nil || (valid != nil && !valid()) {
		c.mu.Unlock()
		return nil
	}
	c.err = err
	w := c.wire
	c.wire = nil
	c.opts.SendBudget.release(len(c.txBuf))
	if !errors.Is(err, io.EOF) {
		c.opts.ReceiveBudget.release(c.rxReserved)
		c.rxReserved = 0
		c.rxBuf = nil
	}
	c.txBuf = nil
	c.notify()
	close(c.done)
	hook := c.onClose
	c.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
	if hook != nil {
		hook()
	}
	return nil
}
func (c *Conn) Close() error {
	// Complete queued writes before an orderly close. Cancellation remains
	// bounded even when the peer or its application has stopped consuming.
	until := time.Now().Add(c.opts.FailureTimeout)
	c.mu.Lock()
	c.txFIN = true
	c.notify()
	c.mu.Unlock()
	for {
		c.mu.Lock()
		pending := c.err == nil && c.txAck < c.txNext
		ch := c.changed
		c.mu.Unlock()
		if !pending || wait(ch, until) != nil {
			break
		}
	}
	err := c.finish(net.ErrClosed, true)
	c.discardReceive()
	return err
}
func (c *Conn) discardReceive() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opts.ReceiveBudget.release(c.rxReserved)
	c.rxReserved = 0
	c.rxBuf = nil
	c.notify()
}
func (c *Conn) Abort() error {
	err := c.finish(net.ErrClosed, false)
	c.discardReceive()
	return err
}
func (c *Conn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.txFIN = true
	c.notify()
	return nil
}

func wait(ch <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		<-ch
		return nil
	}
	d := time.Until(deadline)
	if d <= 0 {
		return os.ErrDeadlineExceeded
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-t.C:
		return os.ErrDeadlineExceeded
	}
}
func (c *Conn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		if !c.rd.IsZero() && !time.Now().Before(c.rd) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if len(c.rxBuf) > 0 {
			n := copy(b, c.rxBuf)
			c.rxBuf = c.rxBuf[n:]
			if len(c.rxBuf) == 0 {
				c.rxBuf = nil
			}
			reserved := minInt(n, c.rxReserved)
			c.rxReserved -= reserved
			c.opts.ReceiveBudget.release(reserved)
			c.rxRead += uint64(n)
			c.notify()
			c.mu.Unlock()
			return n, nil
		}
		if c.rxFIN {
			c.mu.Unlock()
			return 0, io.EOF
		}
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return 0, err
		}
		ch, d := c.changed, c.rd
		c.mu.Unlock()
		if err := wait(ch, d); err != nil {
			return 0, err
		}
	}
}
func (c *Conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	written := 0
	for len(b) > 0 {
		c.mu.Lock()
		if !c.wd.IsZero() && !time.Now().Before(c.wd) {
			c.mu.Unlock()
			return written, os.ErrDeadlineExceeded
		}
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return written, err
		}
		if c.txFIN {
			c.mu.Unlock()
			return written, io.ErrClosedPipe
		}
		room := c.opts.BufferBytes - int(c.txNext-c.peerRead)
		if room > 0 {
			n := len(b)
			if n > room {
				n = room
			}
			if n > MaxPayload {
				n = MaxPayload
			}
			granted, budgetChanged := c.opts.SendBudget.acquire(n)
			if granted == 0 {
				ch, deadline := c.changed, c.wd
				c.mu.Unlock()
				if err := waitBudget(ch, budgetChanged, deadline); err != nil {
					return written, err
				}
				continue
			}
			n = granted
			c.txBuf = append(c.txBuf, b[:n]...)
			c.txNext += uint64(n)
			written += n
			b = b[n:]
			c.notify()
			c.mu.Unlock()
			continue
		}
		ch, d := c.changed, c.wd
		c.mu.Unlock()
		if err := wait(ch, d); err != nil {
			return written, err
		}
	}
	return written, nil
}
func (c *Conn) LocalAddr() net.Addr  { c.mu.Lock(); defer c.mu.Unlock(); return c.local }
func (c *Conn) RemoteAddr() net.Addr { c.mu.Lock(); defer c.mu.Unlock(); return c.remote }
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rd = t
	c.wd = t
	c.notify()
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rd = t
	c.notify()
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wd = t
	c.notify()
	return nil
}

func (c *Conn) mac(direction byte, p []byte) []byte {
	derive := hmac.New(sha256.New, c.key[:])
	derive.Write([]byte("sntp-v3-data-key"))
	derive.Write(c.id[:])
	derive.Write([]byte{direction})
	m := hmac.New(sha256.New, derive.Sum(nil))
	m.Write(p)
	return m.Sum(nil)
}
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func (c *Conn) send(kind byte) error {
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	c.mu.Lock()
	if c.err != nil || c.wire == nil || !c.ready {
		c.mu.Unlock()
		return nil
	}
	w, epoch := c.wire, c.epoch
	seq := c.txSent
	var payload []byte
	if kind == dataPacket {
		off := int(seq - c.txAck)
		if off < 0 || off >= len(c.txBuf) {
			c.mu.Unlock()
			return nil
		}
		n := len(c.txBuf) - off
		if n > MaxPayload {
			n = MaxPayload
		}
		payload = c.txBuf[off : off+n]
	}
	p := make([]byte, headerSize+len(payload))
	p[0] = kind
	if c.rxFIN {
		p[1] = 1
	}
	binary.BigEndian.PutUint64(p[2:10], epoch)
	binary.BigEndian.PutUint64(p[10:18], seq)
	binary.BigEndian.PutUint64(p[18:26], c.rxNext)
	binary.BigEndian.PutUint64(p[26:34], c.rxRead)
	binary.BigEndian.PutUint32(p[34:38], uint32(len(payload)))
	copy(p[headerSize:], payload)
	c.mu.Unlock()
	p = append(p, c.mac(c.side, p)...)
	_ = w.SetWriteDeadline(time.Now().Add(c.opts.FailureTimeout))
	err := writeAll(w, p)
	if err != nil {
		c.detach(w, epoch)
		return err
	}
	c.mu.Lock()
	if c.wire == w && c.epoch == epoch && kind == dataPacket {
		c.txSent = seq + uint64(len(payload))
	}
	c.mu.Unlock()
	return nil
}
func (c *Conn) receive(w net.Conn, epoch uint64) {
	for {
		_ = w.SetReadDeadline(time.Now().Add(c.opts.FailureTimeout))
		head := make([]byte, headerSize)
		if _, err := io.ReadFull(w, head); err != nil {
			c.detach(w, epoch)
			return
		}
		n := int(binary.BigEndian.Uint32(head[34:38]))
		if n > MaxPayload {
			c.finishIf(errors.New("oversized V3 packet"), false, func() bool { return c.wire == w && c.epoch == epoch })
			return
		}
		body := make([]byte, n+sha256.Size)
		if _, err := io.ReadFull(w, body); err != nil {
			c.detach(w, epoch)
			return
		}
		authenticated := append(head, body[:n]...)
		c.mu.Lock()
		if c.wire != w || c.epoch != epoch || c.err != nil {
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if !hmac.Equal(body[n:], c.mac(c.side^1, authenticated)) || binary.BigEndian.Uint64(head[2:10]) != epoch {
			c.finishIf(errors.New("invalid V3 authentication"), false, func() bool { return c.wire == w && c.epoch == epoch })
			return
		}
		seq, ack, read := binary.BigEndian.Uint64(head[10:18]), binary.BigEndian.Uint64(head[18:26]), binary.BigEndian.Uint64(head[26:34])
		c.mu.Lock()
		if c.wire != w || c.epoch != epoch || c.err != nil {
			c.mu.Unlock()
			return
		}
		err := c.checkPeer(ack, read)
		if head[1] > 1 || (head[0] == dataPacket && n == 0) || (head[0] == closePacket && n != 0) {
			err = errors.New("invalid V3 packet flags or size")
		}
		if err == nil {
			c.advancePeer(ack, read)
			if head[1]&1 != 0 && c.txFIN && ack == c.txNext {
				c.finAck = true
			}
			switch head[0] {
			case dataPacket:
				end := seq + uint64(n)
				if end < seq || seq > c.rxNext || (c.rxFIN && end > c.rxNext) {
					err = errors.New("out of order V3 data")
				} else if end > c.rxNext {
					skip := int(c.rxNext - seq)
					if len(c.rxBuf)+n-skip > c.opts.BufferBytes {
						err = errors.New("V3 receive window exceeded")
					} else {
						amount := n - skip
						granted, _ := c.opts.ReceiveBudget.acquire(amount)
						if granted < amount {
							c.opts.ReceiveBudget.release(granted)
							err = errors.New("V3 node receive budget exhausted")
						} else {
							c.rxReserved += amount
							c.rxBuf = append(c.rxBuf, body[skip:n]...)
							c.rxNext = end
						}
					}
				}
			case ackPacket:
				if n != 0 {
					err = errors.New("invalid V3 ACK")
				}
			case finPacket:
				if n != 0 || seq != c.rxNext {
					err = errors.New("invalid V3 FIN")
				} else {
					c.rxFIN = true
				}
			case closePacket:
				err = io.EOF
			default:
				err = errors.New("unknown V3 packet")
			}
		}
		// ACK-only packets must not trigger an infinite ACK ping-pong loop.
		close(c.changed)
		c.changed = make(chan struct{})
		if head[0] != ackPacket || c.txSent < c.txNext {
			select {
			case c.kick <- struct{}{}:
			default:
			}
		}
		c.mu.Unlock()
		if err != nil {
			c.finishIf(err, false, func() bool { return c.wire == w && c.epoch == epoch })
			return
		}
	}
}

func (c *Conn) run() {
	ticker := time.NewTicker(c.opts.Heartbeat)
	defer ticker.Stop()
	recovering := false
	for {
		select {
		case <-c.done:
			return
		case <-c.kick:
		case <-ticker.C:
		}
		c.mu.Lock()
		dead := c.err != nil
		detached := c.detached
		wire := c.wire
		ready := c.ready
		resume := c.resume
		c.mu.Unlock()
		if dead {
			return
		}
		if !detached.IsZero() && time.Since(detached) >= c.opts.Window {
			c.finishIf(errors.New("V3 recovery window expired"), false, func() bool { return !c.detached.IsZero() && time.Since(c.detached) >= c.opts.Window })
			// A concurrent attach may have made the expiry snapshot obsolete.
			if c.Err() != nil {
				return
			}
			continue
		}
		if wire == nil && resume != nil && !recovering {
			recovering = true
			until := detached.Add(c.opts.Window)
			ctx, cancel := context.WithDeadline(context.Background(), until)
			go func() {
				select {
				case <-c.done:
					cancel()
				case <-ctx.Done():
				}
			}()
			for ctx.Err() == nil {
				w, e, a, r, err := resume(ctx, c)
				if err == nil {
					err = c.Attach(w, e, a, r)
				}
				if err == nil {
					break
				}
				if w != nil {
					_ = w.Close()
				}
				timer := time.NewTimer(100 * time.Millisecond)
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
			}
			cancel()
			recovering = false
			continue
		}
		if wire != nil && ready {
			for {
				c.mu.Lock()
				pending := c.txSent < c.txNext
				c.mu.Unlock()
				if !pending {
					break
				}
				if err := c.send(dataPacket); err != nil {
					break
				}
				c.mu.Lock()
				active := c.wire != nil && c.ready
				c.mu.Unlock()
				if !active {
					break
				}
			}
			c.mu.Lock()
			fin := c.txFIN && !c.finAck && c.txSent == c.txNext
			c.mu.Unlock()
			if fin {
				_ = c.send(finPacket)
			} else {
				_ = c.send(ackPacket)
			}
		}
	}
}
