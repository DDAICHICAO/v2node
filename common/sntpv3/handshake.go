package sntpv3

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/hkdf"
)

func DeriveKey(auth, forward, salt, transcript []byte) (key [32]byte, err error) {
	material := append(append(append([]byte(nil), auth...), forward...), transcript...)
	_, err = io.ReadFull(hkdf.New(sha256.New, material, salt, []byte("sntp-eclipse-v3-resume")), key[:])
	return
}
func proof(key [32]byte, label string, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte(label))
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}
func WriteWelcome(w net.Conn, id [16]byte, key [32]byte, opts Options) error {
	opts = opts.normalized()
	b := make([]byte, 32)
	copy(b, id[:])
	binary.BigEndian.PutUint32(b[16:20], uint32(opts.Window/time.Millisecond))
	binary.BigEndian.PutUint32(b[20:24], uint32(opts.BufferBytes))
	binary.BigEndian.PutUint64(b[24:32], uint64(time.Now().UnixMilli()))
	return writeAll(w, append(b, proof(key, "sntp-v3-welcome", b)...))
}
func ReadWelcome(w net.Conn, key [32]byte) (id [16]byte, opts Options, err error) {
	b := make([]byte, 64)
	if _, err = io.ReadFull(w, b); err != nil {
		return
	}
	if !hmac.Equal(b[32:], proof(key, "sntp-v3-welcome", b[:32])) {
		err = errors.New("invalid V3 welcome proof")
		return
	}
	copy(id[:], b[:16])
	opts.Window = time.Duration(binary.BigEndian.Uint32(b[16:20])) * time.Millisecond
	opts.BufferBytes = int(binary.BigEndian.Uint32(b[20:24]))
	opts.ClockOffset = time.Duration(int64(binary.BigEndian.Uint64(b[24:32]))-time.Now().UnixMilli()) * time.Millisecond
	if opts.Window < time.Second || opts.Window > 120*time.Second || opts.BufferBytes < MaxPayload || opts.BufferBytes > 4<<20 {
		err = errors.New("invalid V3 session limits")
	}
	return
}

// Datagram timestamps use the authenticated node clock. Expired datagrams are
// decrypted/acknowledged to preserve cipher counters, then discarded once.
func StampDatagram(p []byte, offset time.Duration) []byte {
	b := make([]byte, 8+len(p))
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().Add(offset).UnixMilli()))
	copy(b[8:], p)
	return b
}
func UnstampDatagram(p []byte, offset time.Duration) ([]byte, bool) {
	if len(p) < 8 {
		return nil, false
	}
	age := time.Now().Add(offset).UnixMilli() - int64(binary.BigEndian.Uint64(p[:8]))
	return p[8:], age >= -5000 && age <= 2000
}
func WriteReady(w net.Conn, id [16]byte, key [32]byte) error {
	return writeAll(w, proof(key, "sntp-v3-ready", id[:]))
}
func ReadReady(w net.Conn, id [16]byte, key [32]byte) error {
	p := make([]byte, 32)
	if _, err := io.ReadFull(w, p); err != nil {
		return err
	}
	if !hmac.Equal(p, proof(key, "sntp-v3-ready", id[:])) {
		return errors.New("invalid V3 ready proof")
	}
	return nil
}

// RequestResume follows the resume selector byte already consumed by the node.
// The session identifier and proofs are binary WSS content, never URL values.
func RequestResume(w net.Conn, c *Conn) (epoch, ack, read uint64, err error) {
	challenge := make([]byte, 32)
	if _, err = io.ReadFull(w, challenge); err != nil {
		return
	}
	epoch = c.NextEpoch()
	rx, consumed := c.Progress()
	req := make([]byte, 40)
	copy(req, c.id[:])
	binary.BigEndian.PutUint64(req[16:24], epoch)
	binary.BigEndian.PutUint64(req[24:32], rx)
	binary.BigEndian.PutUint64(req[32:40], consumed)
	signed := append(req, proof(c.key, "sntp-v3-resume-client", challenge, req)...)
	if err = writeAll(w, signed); err != nil {
		return
	}
	reply := make([]byte, 56)
	if _, err = io.ReadFull(w, reply); err != nil {
		return
	}
	if binary.BigEndian.Uint64(reply[:8]) != epoch || !hmac.Equal(reply[24:], proof(c.key, "sntp-v3-resume-server", challenge, signed, reply[:24])) {
		err = errors.New("invalid V3 resume server proof")
		return
	}
	ack = binary.BigEndian.Uint64(reply[8:16])
	read = binary.BigEndian.Uint64(reply[16:24])
	return
}
func AcceptResume(w net.Conn, lookup func([16]byte) *Conn, authorize ...func() error) error {
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return err
	}
	if err := writeAll(w, challenge); err != nil {
		return err
	}
	req := make([]byte, 72)
	if _, err := io.ReadFull(w, req); err != nil {
		return err
	}
	var id [16]byte
	copy(id[:], req[:16])
	c := lookup(id)
	if c == nil || !hmac.Equal(req[40:], proof(c.key, "sntp-v3-resume-client", challenge, req[:40])) {
		return errors.New("V3 session unavailable")
	}
	for _, check := range authorize {
		if err := check(); err != nil {
			return err
		}
	}
	epoch, ack, read := binary.BigEndian.Uint64(req[16:24]), binary.BigEndian.Uint64(req[24:32]), binary.BigEndian.Uint64(req[32:40])
	return c.attach(w, epoch, ack, read, func(rx, consumed uint64) error {
		reply := make([]byte, 24)
		binary.BigEndian.PutUint64(reply[:8], epoch)
		binary.BigEndian.PutUint64(reply[8:16], rx)
		binary.BigEndian.PutUint64(reply[16:24], consumed)
		return writeAll(w, append(reply, proof(c.key, "sntp-v3-resume-server", challenge, req, reply)...))
	})
}
