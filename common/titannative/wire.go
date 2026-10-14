// Package titannative implements the SNTP native-1 record contract. Keep this
// file byte-identical to the client transport/titannative/wire.go.
package titannative

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/big"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	Mode       = "sntp-eclipse-v3-native-1"
	Capability = "titan_v3_native_v1"
	MaxPayload = 16382
	MaxHello   = 1264
	headerSize = 18
)

var ErrInvalid = errors.New("invalid native record")

// Cipher is one direction, with separate keys for authenticated length/content.
// Any I/O or authentication failure permanently poisons the direction.
type Cipher struct {
	mu           sync.Mutex
	length, body cipher.AEAD
	counter      uint64
	failed       bool
}

func derive(secret, salt []byte, phase string) (*Cipher, *Cipher, error) {
	keys := make([]byte, 128)
	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, salt, []byte(Mode+"/"+phase)), keys); err != nil {
		return nil, nil, err
	}
	a := make([]cipher.AEAD, 4)
	for i := range a {
		var err error
		a[i], err = chacha20poly1305.NewX(keys[i*32 : (i+1)*32])
		if err != nil {
			return nil, nil, err
		}
	}
	return &Cipher{length: a[0], body: a[1]}, &Cipher{length: a[2], body: a[3]}, nil
}

func nonce(n uint64) []byte { b := make([]byte, 24); binary.BigEndian.PutUint64(b[16:], n); return b }
func aadBody(aad, header []byte) []byte {
	out := make([]byte, 0, len(aad)+len(header))
	out = append(out, aad...)
	return append(out, header...)
}

// RandomPadding returns an unbiased random value in [0, max].
func RandomPadding(max int) (int, error) {
	if max < 0 {
		return 0, ErrInvalid
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)+1))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// seal is called with the direction exclusively owned or mu held.
func (c *Cipher) seal(payload, aad []byte) ([]byte, error) {
	if c.failed || c.counter == math.MaxUint64 || len(payload) > MaxPayload {
		c.failed = true
		return nil, ErrInvalid
	}
	padMax := MaxPayload - len(payload)
	if padMax > 255 {
		padMax = 255
	}
	pad, err := RandomPadding(padMax)
	if err != nil {
		c.failed = true
		return nil, err
	}
	plain := make([]byte, 2+len(payload)+pad)
	binary.BigEndian.PutUint16(plain, uint16(len(payload)))
	copy(plain[2:], payload)
	if _, err = rand.Read(plain[2+len(payload):]); err != nil {
		c.failed = true
		return nil, err
	}
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(plain)+16))
	n := nonce(c.counter)
	header := c.length.Seal(nil, n, size[:], aad)
	packet := c.body.Seal(header, n, plain, aadBody(aad, header))
	c.counter++
	return packet, nil
}

func WriteAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if n < 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (c *Cipher) Write(w io.Writer, payload, aad []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	packet, err := c.seal(payload, aad)
	if err == nil {
		err = WriteAll(w, packet)
	}
	if err != nil {
		c.failed = true
	}
	return err
}

func (c *Cipher) Read(r io.Reader, aad []byte, max int) (payload []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		if err != nil {
			c.failed = true
		}
	}()
	if c.failed || c.counter == math.MaxUint64 || max < 0 || max > MaxPayload {
		return nil, ErrInvalid
	}
	var header [headerSize]byte
	if _, err = io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := nonce(c.counter)
	plainLength, err := c.length.Open(nil, n, header[:], aad)
	if err != nil {
		return nil, ErrInvalid
	}
	size := int(binary.BigEndian.Uint16(plainLength))
	limit := max + 2 + 255 + 16
	if limit > MaxPayload+2+16 {
		limit = MaxPayload + 2 + 16
	}
	if size < 18 || size > limit {
		return nil, ErrInvalid
	}
	sealed := make([]byte, size)
	if _, err = io.ReadFull(r, sealed); err != nil {
		return nil, err
	}
	plain, err := c.body.Open(nil, n, sealed, aadBody(aad, header[:]))
	if err != nil {
		return nil, ErrInvalid
	}
	size = int(binary.BigEndian.Uint16(plain))
	if size > max || size > len(plain)-2 || len(plain)-2-size > 255 {
		return nil, ErrInvalid
	}
	c.counter++
	return plain[2 : 2+size], nil
}

// IsV2Length is ONLY a parser discriminator; authentication remains mandatory.
func IsV2Length(b []byte) bool {
	if len(b) < 2 {
		return false
	}
	switch binary.BigEndian.Uint16(b) {
	case 512, 768, 1024, 1280:
		return true
	}
	return false
}

type transcriptReader struct {
	io.Reader
	hash io.Writer
}

func (r transcriptReader) Read(b []byte) (int, error) {
	n, e := r.Reader.Read(b)
	if n > 0 {
		_, _ = r.hash.Write(b[:n])
	}
	return n, e
}

type ServerHandshake struct {
	Hello  []byte
	auth   []byte
	client *ecdh.PublicKey
	reply  *Cipher
	digest []byte
	used   bool
}

// Accept consumes a V3 hello only; the caller authenticates Hello before Reply.
func Accept(r io.Reader, key *ecdh.PrivateKey) (*ServerHandshake, error) {
	var h [48]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return nil, err
	}
	pub, err := ecdh.X25519().NewPublicKey(h[:32])
	if err != nil {
		return nil, ErrInvalid
	}
	auth, err := key.ECDH(pub)
	if err != nil {
		return nil, ErrInvalid
	}
	rx, tx, err := derive(auth, h[32:], "handshake")
	if err != nil {
		return nil, err
	}
	digest := sha256.New()
	_, _ = digest.Write(h[:])
	hello, err := rx.Read(transcriptReader{r, digest}, h[:], MaxHello)
	if err != nil {
		return nil, err
	}
	return &ServerHandshake{Hello: hello, auth: auth, client: pub, reply: tx, digest: digest.Sum(nil)}, nil
}

func traffic(auth, fs, digest, serverPublic []byte) (*Cipher, *Cipher, error) {
	input := append(append([]byte(nil), auth...), fs...)
	h := sha256.New()
	_, _ = h.Write(digest)
	_, _ = h.Write(serverPublic)
	return derive(input, h.Sum(nil), "traffic")
}

func (s *ServerHandshake) Reply(w io.Writer) (*Cipher, *Cipher, error) {
	if s.used {
		return nil, nil, ErrInvalid
	}
	s.used = true
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	fs, err := ephemeral.ECDH(s.client)
	if err != nil {
		return nil, nil, err
	}
	pub := ephemeral.PublicKey().Bytes()
	rx, tx, err := traffic(s.auth, fs, s.digest, pub)
	if err != nil {
		return nil, nil, err
	}
	reply := append([]byte{3, 1}, pub...)
	if err = s.reply.Write(w, reply, s.digest); err != nil {
		return nil, nil, err
	}
	return rx, tx, nil
}

func Connect(rw io.ReadWriter, server *ecdh.PublicKey, hello []byte) (*Cipher, *Cipher, error) {
	if len(hello) > MaxHello {
		return nil, nil, ErrInvalid
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	auth, err := ephemeral.ECDH(server)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		h := make([]byte, 48)
		copy(h, ephemeral.PublicKey().Bytes())
		if _, err = rand.Read(h[32:]); err != nil {
			return nil, nil, err
		}
		tx, rx, err := derive(auth, h[32:], "handshake")
		if err != nil {
			return nil, nil, err
		}
		packet, err := tx.seal(hello, h)
		if err != nil {
			return nil, nil, err
		}
		if IsV2Length(packet) {
			continue
		}
		first := append(h, packet...)
		digest := sha256.Sum256(first)
		if err = WriteAll(rw, first); err != nil {
			return nil, nil, err
		}
		reply, err := rx.Read(rw, digest[:], 34)
		if err != nil {
			return nil, nil, err
		}
		if len(reply) != 34 || reply[0] != 3 || reply[1] != 1 {
			return nil, nil, ErrInvalid
		}
		pub, err := ecdh.X25519().NewPublicKey(reply[2:])
		if err != nil {
			return nil, nil, ErrInvalid
		}
		fs, err := ephemeral.ECDH(pub)
		if err != nil {
			return nil, nil, ErrInvalid
		}
		return traffic(auth, fs, digest[:], reply[2:])
	}
	return nil, nil, ErrInvalid
}
