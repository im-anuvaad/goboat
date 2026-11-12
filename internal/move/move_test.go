package move

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

type fakeOld struct {
	mu        sync.Mutex
	src       *Source
	srv       *httptest.Server
	dataDir   string
	staging   string
	stopped   bool
	restarted []string
	moved     *Moved
}

func newFakeOld(t *testing.T) *fakeOld {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "goboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secrets.LoadOrCreate(filepath.Join(dir, "master.key"))
	f := &fakeOld{dataDir: t.TempDir(), staging: t.TempDir()}
	os.WriteFile(filepath.Join(f.dataDir, "upload.bin"), bytes.Repeat([]byte("u"), 3000), 0o600)
	f.src = &Source{Store: st, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "v1",
		Dashboard: func() string { return "goboat.example.test" },
		Hooks: SourceHooks{
			Apps: func() []AppInfo {
				return []AppInfo{{Name: "web", Commit: strings.Repeat("a", 40), Branch: "main", Running: true}}
			},
			Addrs:  func() []string { return []string{"192.0.2.1", "2001:db8::1"} },
			Bundle: func(ctx context.Context, w io.Writer) error { _, err := w.Write([]byte("settings")); return err },
			Files: func(ctx context.Context, app string, have []FileInfo, w io.Writer) error {
				_, _, err := PackDataDir(ctx, f.dataDir, f.staging, have, w)
				return err
			},
			Stop: func(ctx context.Context) ([]string, error) {
				f.mu.Lock()
				f.stopped = true
				f.mu.Unlock()
				return []string{"web"}, nil
			},
			Restart: func(ctx context.Context, running []string) {
				f.mu.Lock()
				f.restarted, f.stopped = running, false
				f.mu.Unlock()
			},
			Moved: func(m Moved) {
				f.mu.Lock()
				f.moved = &m
				f.mu.Unlock()
			},
			Allow:    func(string) error { return nil },
			Fail:     func(string) {},
			ClientIP: func(*http.Request) string { return "198.51.100.2" },
		}}
	f.srv = httptest.NewTLSServer(f.src.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

type fakeNew struct {
	mu       sync.Mutex
	accepted []string
	dataDir  string
	prepared []string
	started  []string
}

func (n *fakeNew) hooks() TargetHooks {
	return TargetHooks{
		Prepare: func(ctx context.Context, app, commit, branch string) error {
			n.mu.Lock()
			n.prepared = append(n.prepared, app+"@"+commit[:7]+"/"+branch)
			n.mu.Unlock()
			return nil
		},
		DataDir: func(string) (string, int, int, error) { return n.dataDir, os.Getuid(), os.Getgid(), nil },
		Start: func(ctx context.Context, app string) error {
			n.mu.Lock()
			n.started = append(n.started, app)
			n.mu.Unlock()
			return nil
		},
		Accept: func(addrs []string) {
			n.mu.Lock()
			n.accepted = addrs
			n.mu.Unlock()
		},
		Addrs: func() []string { return []string{"198.51.100.2"} },
	}
}

func client(t *testing.T, f *fakeOld, code string) *Client {
	t.Helper()
	c, err := NewClient(f.srv.URL, code)
	if err != nil {
		t.Fatal(err)
	}
	c.http = f.srv.Client()
	return c
}

func waitPhase(t *testing.T, s *Source, p Phase) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if st := s.State(); st.Phase == p && (p != PhaseSwitching || st.Stopped) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("phase %q, want %q", s.State().Phase, p)
}

func TestMoveEndToEnd(t *testing.T) {
	ctx := context.Background()
	old := newFakeOld(t)
	code, err := old.src.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client(t, old, NewCode()).Hello(ctx); err == nil {
		t.Fatal("a wrong code connected")
	}
	c := client(t, old, code)
	var got bytes.Buffer
	if err := c.Bundle(ctx, func(r io.Reader) error { _, err := io.Copy(&got, r); return err }); err == nil {
		t.Fatal("bundle before hello")
	}
	if _, err := c.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	if !old.src.Frozen() {
		t.Fatal("configuration not frozen once the new server connected")
	}
	if err := c.Bundle(ctx, func(r io.Reader) error { _, err := io.Copy(&got, r); return err }); err != nil || got.String() != "settings" {
		t.Fatalf("bundle: %q %v", got.String(), err)
	}

	nw := &fakeNew{dataDir: t.TempDir()}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, "v1", nw.hooks(), slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	waitPhase(t, old.src, PhaseReady)
	if _, err := os.Stat(filepath.Join(nw.dataDir, "upload.bin")); err != nil {
		t.Fatal("not pre-copied before the switch")
	}
	// A file written after the pre-copy arrives with the final copy.
	os.WriteFile(filepath.Join(old.dataDir, "late.txt"), []byte("late"), 0o600)
	if err := old.src.Switch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var moved *Moved
	for i := 0; i < 100 && moved == nil; i++ { // retiring follows the answer
		time.Sleep(50 * time.Millisecond)
		old.mu.Lock()
		moved = old.moved
		old.mu.Unlock()
	}
	if st := old.src.State(); st.Phase != PhaseDone || moved == nil || moved.IP != "198.51.100.2" {
		t.Fatalf("old server after the move: %+v moved=%+v", st.Phase, old.moved)
	}
	if strings.Join(moved.Forward, ",") != "198.51.100.2" || strings.Join(nw.accepted, ",") != "192.0.2.1,2001:db8::1" {
		t.Fatalf("addresses: old forwards to %v, new accepts %v", old.moved.Forward, nw.accepted)
	}
	if strings.Join(nw.prepared, ",") != "web@aaaaaaa/main" || strings.Join(nw.started, ",") != "web" {
		t.Fatalf("new server: prepared %v, started %v", nw.prepared, nw.started)
	}
	if b, _ := os.ReadFile(filepath.Join(nw.dataDir, "late.txt")); string(b) != "late" {
		t.Fatal("the final copy missed a late file")
	}
	if _, err := c.Hello(ctx); err != nil {
		t.Fatalf("hello after done (a restarted new server finishing): %v", err)
	}
	if err := c.Files(ctx, "web", false, nil, func(io.Reader) error { return nil }); err == nil {
		t.Fatal("data served after the move")
	}
}

func TestMoveCancelAndFailure(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Cancelled while the new server waits for Switch now.
	old := newFakeOld(t)
	code, _ := old.src.Start(ctx)
	c := client(t, old, code)
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c, "v1", (&fakeNew{dataDir: t.TempDir()}).hooks(), quiet) }()
	waitPhase(t, old.src, PhaseReady)
	if err := old.src.Cancel(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrCancelled) {
		t.Fatalf("run after cancel: %v", err)
	}
	if old.src.Frozen() {
		t.Fatal("still frozen after cancel")
	}

	// The new server fails after the switch: the old one restarts its apps.
	old = newFakeOld(t)
	code, _ = old.src.Start(ctx)
	c = client(t, old, code)
	nw := &fakeNew{dataDir: t.TempDir()}
	h := nw.hooks()
	h.Start = func(context.Context, string) error { return errors.New("health check failed") }
	go func() { done <- Run(ctx, c, "v1", h, quiet) }()
	waitPhase(t, old.src, PhaseReady)
	old.src.Switch(ctx)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "health check failed") {
		t.Fatalf("run: %v", err)
	}
	c.Failed(ctx, err.Error())
	time.Sleep(100 * time.Millisecond)
	old.mu.Lock()
	restarted := old.restarted
	old.mu.Unlock()
	if old.src.State().Phase != PhaseFailed || strings.Join(restarted, ",") != "web" {
		t.Fatalf("after a failure: phase %q, restarted %v", old.src.State().Phase, restarted)
	}

	// Versions must match.
	old = newFakeOld(t)
	code, _ = old.src.Start(ctx)
	if err := Run(ctx, client(t, old, code), "v2", nw.hooks(), quiet); err == nil || !strings.Contains(err.Error(), "update the old server first") {
		t.Fatalf("version mismatch: %v", err)
	}

	// goboat restarting on the old server mid-move ends the move.
	old.src.Load(ctx)
	if st := old.src.State(); st.Phase != PhaseFailed || old.src.Frozen() {
		t.Fatalf("after a restart: %q", st.Phase)
	}
}
