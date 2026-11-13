package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

// fakeCF is the part of the Cloudflare API goboat uses, for one zone
// (example.test, id z1), keeping state like Cloudflare does.
type fakeCF struct {
	t  *testing.T
	mu sync.Mutex
	n  int

	dns     map[string]fakeRecord // id ->
	certs   map[string]fakeCert   // id -> (not revoked)
	aop     map[string]string     // id -> status
	aopOn   bool
	failDNS bool // DNS calls answer 500 (Cloudflare having a bad day)

	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
}

type fakeRecord struct{ Type, Name, Content string }
type fakeCert struct {
	Hostnames []string
	PEM       string
}

func newFakeCF(t *testing.T) (*fakeCF, *httptest.Server) {
	f := &fakeCF{t: t, dns: map[string]fakeRecord{}, certs: map[string]fakeCert{}, aop: map[string]string{}}
	f.caKey, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "fake origin CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &f.caKey.PublicKey, f.caKey)
	f.caCert, _ = x509.ParseCertificate(der)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeCF) id() string { f.n++; return fmt.Sprintf("id%03d", f.n) }

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": v})
	}
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1, "message": msg}}})
	}
	p, q := r.URL.Path, r.URL.Query()
	seg := strings.Split(strings.Trim(p, "/"), "/")
	switch {
	case p == "/zones" && q.Get("name") != "":
		if q.Get("name") == "example.test" {
			ok([]map[string]string{{"id": "z1", "name": "example.test", "status": "active"}})
		} else {
			ok([]any{})
		}
	case strings.HasPrefix(p, "/zones/z1/dns_records"):
		if f.failDNS {
			fail(500, "internal error")
			return
		}
		switch r.Method {
		case http.MethodGet:
			var out []map[string]any
			for id, rec := range f.dns {
				if rec.Name == q.Get("name") {
					out = append(out, map[string]any{"id": id, "type": rec.Type, "name": rec.Name, "content": rec.Content, "proxied": true})
				}
			}
			ok(out)
		case http.MethodPost, http.MethodPatch:
			var body struct{ Type, Name, Content string }
			json.NewDecoder(r.Body).Decode(&body)
			id := ""
			if r.Method == http.MethodPatch {
				id = seg[len(seg)-1]
				old := f.dns[id]
				if body.Type == "" {
					body.Type, body.Name = old.Type, old.Name
				}
			} else {
				id = f.id()
			}
			f.dns[id] = fakeRecord{Type: body.Type, Name: body.Name, Content: body.Content}
			ok(map[string]string{"id": id})
		case http.MethodDelete:
			delete(f.dns, seg[len(seg)-1])
			ok(map[string]string{"id": seg[len(seg)-1]})
		}
	case p == "/certificates" && r.Method == http.MethodPost:
		var body struct {
			CSR       string   `json:"csr"`
			Hostnames []string `json:"hostnames"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		b, _ := pem.Decode([]byte(body.CSR))
		csr, err := x509.ParseCertificateRequest(b.Bytes)
		if err != nil {
			fail(400, "bad csr")
			return
		}
		tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: body.Hostnames[0]}, DNSNames: body.Hostnames,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(5475 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
		id := f.id()
		pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		f.certs[id] = fakeCert{Hostnames: body.Hostnames, PEM: pemStr}
		ok(map[string]any{"id": id, "certificate": pemStr, "hostnames": body.Hostnames, "expires_on": "2041-01-01"})
	case p == "/certificates" && r.Method == http.MethodGet:
		var out []map[string]any
		for id, c := range f.certs {
			out = append(out, map[string]any{"id": id, "certificate": c.PEM, "hostnames": c.Hostnames, "expires_on": "2041-01-01"})
		}
		ok(out)
	case strings.HasPrefix(p, "/certificates/") && r.Method == http.MethodDelete:
		id := seg[1]
		if _, have := f.certs[id]; !have {
			fail(404, "not found")
			return
		}
		delete(f.certs, id)
		ok(map[string]string{"id": id})
	case p == "/zones/z1/settings/ssl":
		ok(map[string]string{"value": "strict"})
	case p == "/zones/z1/origin_tls_client_auth/settings":
		if r.Method == http.MethodPut {
			var body struct{ Enabled bool }
			json.NewDecoder(r.Body).Decode(&body)
			f.aopOn = body.Enabled
		}
		ok(map[string]bool{"enabled": f.aopOn})
	case p == "/zones/z1/origin_tls_client_auth":
		if r.Method == http.MethodPost {
			id := f.id()
			f.aop[id] = "active"
			ok(map[string]string{"id": id, "status": "active"})
			return
		}
		var out []map[string]string
		for id, st := range f.aop {
			out = append(out, map[string]string{"id": id, "status": st})
		}
		ok(out)
	case strings.HasPrefix(p, "/zones/z1/origin_tls_client_auth/"):
		id := seg[len(seg)-1]
		st, have := f.aop[id]
		if !have {
			fail(404, "not found")
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.aop, id)
		}
		ok(map[string]string{"id": id, "status": st})
	default:
		f.t.Errorf("fake Cloudflare: unexpected %s %s", r.Method, r.URL)
		fail(404, "unexpected")
	}
}

// state returns the zone's DNS names, certificate hostnames (one entry per
// certificate) and AOP certificate count.
func (f *fakeCF) state() (dns, certs []string, aop int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.dns {
		dns = append(dns, r.Type+" "+r.Name)
	}
	for _, c := range f.certs {
		certs = append(certs, strings.Join(c.Hostnames, ","))
	}
	sort.Strings(dns)
	sort.Strings(certs)
	return dns, certs, len(f.aop)
}

func (f *fakeCF) addCert(host string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.id()
	f.certs[id] = fakeCert{Hostnames: []string{host}}
	return id
}

type testEdge struct {
	m       *Manager
	f       *fakeCF
	knownMu sync.Mutex // background syncs read known
	known   []string
}

func (te *testEdge) setKnown(hosts []string) {
	te.knownMu.Lock()
	te.known = hosts
	te.knownMu.Unlock()
}

func newTestEdge(t *testing.T) *testEdge {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "goboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secrets.LoadOrCreate(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	f, srv := newFakeCF(t)
	te := &testEdge{f: f}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(ctx, Config{Listen: "127.0.0.1:0", AOP: true, DataRoot: dir, CloudflareBase: srv.URL,
		KnownHosts: func(context.Context) ([]string, error) {
			te.knownMu.Lock()
			defer te.knownMu.Unlock()
			return append([]string(nil), te.known...), nil
		}}, st, box, log)
	if err != nil {
		t.Fatal(err)
	}
	m.Router = &Router{Log: log}
	// Background syncs (started by RemoveHost) must end before the temp
	// dirs go.
	t.Cleanup(func() { m.PauseAndWait("test done") })
	sealed, _ := box.Seal([]byte("tok"), purposeCFToken)
	st.SetSetting(ctx, SettingCFToken, sealed)
	m.pubV4, m.pubV6 = "192.0.2.1", "off"
	te.m = m
	return te
}

// serve sets the routed (and configured) hostnames and syncs.
func (te *testEdge) serve(t *testing.T, hosts ...string) {
	t.Helper()
	te.setKnown(hosts)
	var rs []Route
	for i, h := range hosts {
		rs = append(rs, Route{Host: h, App: fmt.Sprintf("app%d", i), Port: 20001 + i})
	}
	te.m.SetRoutes(rs)
	te.sync(t)
}

func (te *testEdge) sync(t *testing.T) error {
	t.Helper()
	_, err := te.m.Sync(context.Background())
	return err
}

func (te *testEdge) remove(t *testing.T, host string) {
	t.Helper()
	var keep []string
	te.knownMu.Lock()
	for _, h := range te.known {
		if h != host {
			keep = append(keep, h)
		}
	}
	te.knownMu.Unlock()
	te.setKnown(keep)
	var rs []Route
	for i, h := range keep {
		rs = append(rs, Route{Host: h, App: fmt.Sprintf("app%d", i), Port: 20001 + i})
	}
	te.m.SetRoutes(rs)
	if err := te.m.RemoveHost(context.Background(), host); err != nil {
		t.Fatal(err)
	}
}

func want(t *testing.T, f *fakeCF, dns, certs string, aop int) {
	t.Helper()
	d, c, a := f.state()
	if strings.Join(d, ";") != dns || strings.Join(c, ";") != certs || a != aop {
		t.Fatalf("Cloudflare state:\n  dns   %q (want %q)\n  certs %q (want %q)\n  aop   %d (want %d)", d, dns, c, certs, a, aop)
	}
}

// Quirk D: deleting an app and adding it again with the same domain, over
// and over, leaves exactly one certificate and one AOP certificate.
func TestDeleteAndRecreateLeavesOneOfEach(t *testing.T) {
	te := newTestEdge(t)
	te.serve(t, "blog.example.test")
	want(t, te.f, "A blog.example.test", "blog.example.test", 1)
	for i := 0; i < 3; i++ {
		te.remove(t, "blog.example.test")
		te.serve(t, "blog.example.test")
	}
	want(t, te.f, "A blog.example.test", "blog.example.test", 1)
	if n := len(te.m.Cleanup(context.Background())); n != 0 {
		t.Fatalf("%d cleanup items left", n)
	}
}

// A removal while Cloudflare fails is kept and done once it works again;
// the last hostname leaving the zone releases it (AOP off, cert deleted).
func TestCleanupSurvivesFailures(t *testing.T) {
	te := newTestEdge(t)
	te.serve(t, "blog.example.test", "shop.example.test")
	te.f.mu.Lock()
	te.f.failDNS = true
	te.f.mu.Unlock()
	te.remove(t, "blog.example.test")
	if err := te.sync(t); err == nil {
		t.Fatal("sync with failing DNS reported no error")
	}
	items := te.m.Cleanup(context.Background())
	if len(items) != 1 || items[0].Host != "blog.example.test" || items[0].Attempts == 0 || items[0].LastError == "" {
		t.Fatalf("cleanup items %+v", items)
	}
	te.f.mu.Lock()
	te.f.failDNS = false
	te.f.mu.Unlock()
	te.sync(t)
	want(t, te.f, "A shop.example.test", "shop.example.test", 1)

	te.remove(t, "shop.example.test")
	te.sync(t)
	want(t, te.f, "", "", 0)
	if te.f.aopOn {
		t.Fatal("zone-level AOP still on after the last hostname left")
	}
	if n := len(te.m.Cleanup(context.Background())); n != 0 {
		t.Fatalf("%d cleanup items left", n)
	}
}

// Leftovers from earlier installs: extra certificates for a served
// hostname and extra AOP certificates are removed; certificates for other
// hostnames are only listed, until the user revokes them.
func TestReconcileLeftovers(t *testing.T) {
	te := newTestEdge(t)
	te.f.addCert("blog.example.test") // an earlier install's certificate
	te.f.addCert("other.example.test")
	te.f.mu.Lock()
	te.f.aop["old1"], te.f.aop["old2"] = "active", "active"
	te.f.aopOn = true
	te.f.mu.Unlock()
	te.serve(t, "blog.example.test")
	want(t, te.f, "A blog.example.test", "blog.example.test;other.example.test", 1)
	u := te.m.Unused()
	if len(u) != 1 || u[0].Hostnames[0] != "other.example.test" {
		t.Fatalf("unused %+v", u)
	}
	if err := te.m.RevokeUnused(context.Background(), "not-listed"); err == nil {
		t.Fatal("revoked a certificate that isn't in the list")
	}
	if err := te.m.RevokeUnused(context.Background(), u[0].ID); err != nil {
		t.Fatal(err)
	}
	want(t, te.f, "A blog.example.test", "blog.example.test", 1)
}

// A configured domain of an app that failed to load is never cleaned up,
// and records that no longer point at this server (moved) are left alone.
func TestCleanupSparesConfiguredAndMovedHosts(t *testing.T) {
	te := newTestEdge(t)
	te.serve(t, "blog.example.test", "shop.example.test")
	// shop's app fails to load: not routed, still configured.
	te.m.SetRoutes([]Route{{Host: "blog.example.test", App: "app0", Port: 20001}})
	te.sync(t)
	want(t, te.f, "A blog.example.test;A shop.example.test", "blog.example.test;shop.example.test", 1)

	// blog moved to another server: its record points there now.
	te.f.mu.Lock()
	for id, r := range te.f.dns {
		if r.Name == "blog.example.test" {
			r.Content = "198.51.100.7"
			te.f.dns[id] = r
		}
	}
	te.f.mu.Unlock()
	te.remove(t, "blog.example.test")
	te.sync(t)
	want(t, te.f, "A blog.example.test;A shop.example.test", "shop.example.test", 1)
}

// A retired server passes connections on, as raw TCP, to the new one.
func TestForwardListener(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	up, _ := net.Listen("tcp", "127.0.0.1:0")
	defer up.Close()
	go func() {
		c, err := up.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		b := make([]byte, 5)
		io.ReadFull(c, b)
		c.Write(append([]byte("new:"), b...))
	}()
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	_, port, _ := net.SplitHostPort(up.Addr().String())
	m := &Manager{log: log, fwdPort: port}
	fl := m.forwarding(ln)
	defer fl.Close()
	m.ForwardTo([]string{"127.0.0.1"}, time.Now().Add(time.Minute))
	go func() {
		c, err := fl.Accept() // only returns connections that are not forwarded
		if err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	b, _ := io.ReadAll(c)
	if string(b) != "new:hello" {
		t.Fatalf("got %q", b)
	}
}
