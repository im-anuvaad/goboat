// Package move moves a working goboat installation to another server
// (docs/architecture.md §10). The old server ("source") serves /move/v1/*
// on its dashboard domain; the new one ("target") is its client. Cloudflare
// terminates TLS in between, so everything is also encrypted with a key
// derived from the one-time move code, which never travels.
package move

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// codeBytes is the entropy of a move code: 128 bits.
const codeBytes = 16

var codeEnc = base32.NewEncoding("abcdefghijkmnpqrstuvwxyz23456789").WithPadding(base32.NoPadding)

// NewCode returns a random move code, e.g. "k3jd-8f2s-…" (26 characters
// in groups of 4).
func NewCode() string {
	b := make([]byte, codeBytes)
	rand.Read(b)
	s := codeEnc.EncodeToString(b)
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), "-")
}

// normalizeCode removes separators and spaces and lowercases.
func normalizeCode(code string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code)))
}

// Keys are what both servers derive from the code.
type Keys struct {
	Token string // sent as Authorization: Bearer
	enc   []byte // never sent
}

// DeriveKeys derives the auth token and the encryption key from a code.
func DeriveKeys(code string) (Keys, error) {
	raw, err := codeEnc.DecodeString(normalizeCode(code))
	if err != nil || len(raw) != codeBytes {
		return Keys{}, errors.New("that is not a move code (copy it from the old server's Move page)")
	}
	tok, err := hkdf.Key(sha256.New, raw, []byte("goboat-move-v1"), "auth token", 32)
	if err != nil {
		return Keys{}, err
	}
	enc, err := hkdf.Key(sha256.New, raw, []byte("goboat-move-v1"), "encryption key", 32)
	if err != nil {
		return Keys{}, err
	}
	return Keys{Token: hex.EncodeToString(tok), enc: enc}, nil
}

// TokenOK compares a presented token in constant time.
func (k Keys) TokenOK(presented string) bool {
	return len(presented) == len(k.Token) && subtle.ConstantTimeCompare([]byte(presented), []byte(k.Token)) == 1
}

func (k Keys) aead() cipher.AEAD {
	b, _ := aes.NewCipher(k.enc)
	g, _ := cipher.NewGCM(b)
	return g
}

// Streams are frames: 4-byte length, then AES-256-GCM of up to frameSize
// bytes, nonce = 8 random bytes per stream + 4-byte counter. The additional
// data binds each frame to the stream's purpose, its position and whether
// it is the last, so frames can't be reordered, replayed into another
// request or cut off without the reader noticing.
const frameSize = 1 << 20

type encWriter struct {
	w       io.Writer
	g       cipher.AEAD
	purpose string
	prefix  [8]byte
	n       uint32
	buf     []byte
	err     error
}

// Encrypt returns a writer that encrypts everything written to it into w.
// Close writes the final frame; without it the reader fails.
func (k Keys) Encrypt(w io.Writer, purpose string) io.WriteCloser {
	e := &encWriter{w: w, g: k.aead(), purpose: purpose, buf: make([]byte, 0, frameSize)}
	rand.Read(e.prefix[:])
	if _, err := w.Write(e.prefix[:]); err != nil {
		e.err = err
	}
	return e
}

func (e *encWriter) aad(last bool) []byte {
	l := byte(0)
	if last {
		l = 1
	}
	return fmt.Appendf(nil, "%s|%d|%d", e.purpose, e.n, l)
}

func (e *encWriter) flush(last bool) error {
	if e.err != nil {
		return e.err
	}
	nonce := make([]byte, 12)
	copy(nonce, e.prefix[:])
	binary.BigEndian.PutUint32(nonce[8:], e.n)
	ct := e.g.Seal(nil, nonce, e.buf, e.aad(last))
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	if _, err := e.w.Write(hdr[:]); err != nil {
		e.err = err
		return err
	}
	if _, err := e.w.Write(ct); err != nil {
		e.err = err
		return err
	}
	e.n++
	e.buf = e.buf[:0]
	return nil
}

func (e *encWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		if e.err != nil {
			return n, e.err
		}
		c := min(len(p), frameSize-len(e.buf))
		e.buf = append(e.buf, p[:c]...)
		p, n = p[c:], n+c
		if len(e.buf) == frameSize {
			if err := e.flush(false); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *encWriter) Close() error { return e.flush(true) }

type decReader struct {
	r       io.Reader
	g       cipher.AEAD
	purpose string
	prefix  [8]byte
	n       uint32
	buf     []byte
	done    bool
	err     error
}

// Decrypt reads a stream written by Encrypt with the same purpose. It
// returns an error, never silently short data, if the stream was changed
// or cut off.
func (k Keys) Decrypt(r io.Reader, purpose string) io.Reader {
	d := &decReader{r: r, g: k.aead(), purpose: purpose}
	if _, err := io.ReadFull(r, d.prefix[:]); err != nil {
		d.err = fmt.Errorf("move stream: %w", err)
	}
	return d
}

func (d *decReader) Read(p []byte) (int, error) {
	for len(d.buf) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		var hdr [4]byte
		if _, err := io.ReadFull(d.r, hdr[:]); err != nil {
			d.err = errors.New("move stream: cut off before its end")
			return 0, d.err
		}
		size := binary.BigEndian.Uint32(hdr[:])
		if size > frameSize+64 {
			d.err = errors.New("move stream: frame too large")
			return 0, d.err
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(d.r, ct); err != nil {
			d.err = errors.New("move stream: cut off before its end")
			return 0, d.err
		}
		nonce := make([]byte, 12)
		copy(nonce, d.prefix[:])
		binary.BigEndian.PutUint32(nonce[8:], d.n)
		for _, last := range []bool{false, true} {
			l := byte(0)
			if last {
				l = 1
			}
			pt, err := d.g.Open(nil, nonce, ct, fmt.Appendf(nil, "%s|%d|%d", d.purpose, d.n, l))
			if err == nil {
				d.buf, d.done = pt, last
				d.n++
				break
			}
			if last {
				d.err = errors.New("move stream: does not decrypt (wrong move code, or changed on the way)")
				return 0, d.err
			}
		}
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

// Seal encrypts a JSON value (small messages).
func (k Keys) Seal(purpose string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w := k.Encrypt(&out, purpose)
	w.Write(b)
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Open decrypts a value sealed with Seal, reading at most max bytes.
func (k Keys) Open(r io.Reader, purpose string, max int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(k.Decrypt(r, purpose), max+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > max {
		return errors.New("move message too large")
	}
	return json.Unmarshal(b, v)
}
