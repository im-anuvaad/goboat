package move

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

func TestCodeAndKeys(t *testing.T) {
	code := NewCode()
	if len(normalizeCode(code)) != 26 || strings.Count(code, "-") != 6 {
		t.Fatalf("code %q", code)
	}
	a, err := DeriveKeys(code)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveKeys(strings.ToUpper(strings.ReplaceAll(code, "-", " ")))
	if err != nil || b.Token != a.Token {
		t.Fatalf("the same code typed differently gives other keys: %v", err)
	}
	if other, _ := DeriveKeys(NewCode()); other.Token == a.Token {
		t.Fatal("two codes, one token")
	}
	if !a.TokenOK(a.Token) || a.TokenOK(a.Token[:10]) || a.TokenOK("") {
		t.Fatal("TokenOK")
	}
	for _, bad := range []string{"", "abc", "0000-0000-0000-0000-0000-0000-00", code + "x"} {
		if _, err := DeriveKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStreamRoundTripAndTampering(t *testing.T) {
	k, _ := DeriveKeys(NewCode())
	data := make([]byte, 3*frameSize+12345) // several frames
	rand.Read(data)
	var buf bytes.Buffer
	w := k.Encrypt(&buf, "bundle")
	w.Write(data[:100])
	w.Write(data[100:])
	w.Close()
	enc := buf.Bytes()

	got, err := io.ReadAll(k.Decrypt(bytes.NewReader(enc), "bundle"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("round trip: %v (%d of %d bytes)", err, len(got), len(data))
	}
	cases := map[string]func() ([]byte, string, Keys){
		"cut off": func() ([]byte, string, Keys) { return enc[:len(enc)-frameSize/2], "bundle", k },
		"last frame cut": func() ([]byte, string, Keys) {
			// Drop the final frame entirely: the reader must not report EOF.
			n := 8
			for i := 0; i < 3; i++ {
				n += 4 + frameSize + 16
			}
			return enc[:n], "bundle", k
		},
		"bit flipped":   func() ([]byte, string, Keys) { c := append([]byte(nil), enc...); c[100] ^= 1; return c, "bundle", k },
		"other purpose": func() ([]byte, string, Keys) { return enc, "files:web", k },
		"other code":    func() ([]byte, string, Keys) { o, _ := DeriveKeys(NewCode()); return enc, "bundle", o },
	}
	for name, f := range cases {
		c, purpose, key := f()
		if _, err := io.ReadAll(key.Decrypt(bytes.NewReader(c), purpose)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}

	// Small messages.
	msg, _ := k.Seal("hello", map[string]string{"v": "x"})
	var out map[string]string
	if err := k.Open(bytes.NewReader(msg), "hello", 1024, &out); err != nil || out["v"] != "x" {
		t.Fatalf("seal/open: %v %v", out, err)
	}
	if err := k.Open(bytes.NewReader(msg), "hello", 3, &out); err == nil {
		t.Fatal("size limit")
	}
	// An empty stream (just the final frame) is fine.
	buf.Reset()
	w = k.Encrypt(&buf, "e")
	w.Close()
	if b, err := io.ReadAll(k.Decrypt(&buf, "e")); err != nil || len(b) != 0 {
		t.Fatalf("empty stream: %q %v", b, err)
	}
}
