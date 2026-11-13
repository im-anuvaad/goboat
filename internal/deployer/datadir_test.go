package deployer

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/logs"
	"github.com/im-anuvaad/goboat/internal/store"
	"github.com/im-anuvaad/goboat/internal/users"
)

// Everything in DATA_DIR survives redeploys, not only the SQLite files that
// get backed up (docs/app-contract.md §8). This runs the on-disk steps of
// several deploys (directory repair, switching `current`, wiping TMPDIR,
// pruning old builds) and checks that DATA_DIR is untouched.
func TestDataDirSurvivesRedeploys(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "goboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// CI runs unprivileged: goboat's own directories belong to the test
	// user instead of root.
	l := layout.Layout{Root: root, UID: os.Getuid(), GID: os.Getgid()}
	d, err := New(ctx, Deps{Store: db, Layout: l})
	if err != nil {
		t.Fatal(err)
	}
	const name = "web"
	u := users.User{Name: "me", UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	lg, err := logs.Open(filepath.Join(root, "deploy.log"), logs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lg.Close() })

	data := map[string][]byte{
		"app.db":                 append([]byte("SQLite format 3\x00"), make([]byte, 100)...),
		"uploads/2026/photo.jpg": []byte("\xff\xd8\xff not really a jpeg"),
		"cache.json":             []byte(`{"k":"v"}`),
		".hidden/state":          []byte("x"),
	}
	if err := l.EnsureApp(name, u); err != nil {
		t.Fatal(err)
	}
	for p, b := range data {
		full := filepath.Join(l.DataDir(name), p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var last string
	for i := 1; i <= 5; i++ {
		if err := l.EnsureApp(name, u); err != nil {
			t.Fatal(err)
		}
		sha := strings.Repeat(strconv.Itoa(i), 40)
		tag := BuildID(sha)
		last = tag
		if err := os.MkdirAll(l.BuildSrc(name, tag), 0o755); err != nil {
			t.Fatal(err)
		}
		b := Build{App: name, ID: tag, GitSHA: sha, Branch: "main", Package: ".", HealthPath: "/", CreatedAt: time.Unix(int64(1000+i), 0)}
		if err := d.insertBuild(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(l.TmpDir(name), "scratch"), []byte("tmp"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := d.swapCurrent(name, tag); err != nil {
			t.Fatal(err)
		}
		if err := l.WipeTmp(name, u); err != nil {
			t.Fatal(err)
		}
		d.pruneBuilds(name, lg)
	}

	for p, want := range data {
		got, err := os.ReadFile(filepath.Join(l.DataDir(name), p))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("DATA_DIR/%s changed by redeploys: %q, %v", p, got, err)
		}
	}
	if left, _ := os.ReadDir(l.TmpDir(name)); len(left) != 0 {
		t.Errorf("TMPDIR not wiped: %v", left)
	}
	if kept, _ := os.ReadDir(l.BuildsDir(name)); len(kept) != KeepBuilds {
		t.Errorf("kept %d builds, want %d", len(kept), KeepBuilds)
	}
	if cur := d.currentBuild(name); cur != last {
		t.Errorf("current = %q, want %s", cur, last)
	}
}
