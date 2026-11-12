package move

import (
	"archive/tar"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Pre-copy, then a final pass that sends only what changed (and the
// databases), deletes what was deleted, and leaves the target equal to the
// source.
func TestDataDirPrecopyAndFinal(t *testing.T) {
	ctx := context.Background()
	src, dst, staging := t.TempDir(), t.TempDir(), t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, "app.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.Exec(`CREATE TABLE t (v TEXT)`)
	db.Exec(`INSERT INTO t VALUES ('one')`)
	os.MkdirAll(filepath.Join(src, "uploads", "2026"), 0o700)
	os.WriteFile(filepath.Join(src, "uploads", "2026", "a.jpg"), bytes.Repeat([]byte("a"), 5000), 0o640)
	os.WriteFile(filepath.Join(src, "gone.txt"), []byte("deleted before the switch"), 0o600)
	uid, gid := os.Getuid(), os.Getgid()

	pass := func(final bool) (int, int64) {
		t.Helper()
		have, err := ListDataDir(dst)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, _, err := PackDataDir(ctx, src, staging, have, &buf); err != nil {
			t.Fatal(err)
		}
		n, b, err := UnpackDataDir(&buf, dst, uid, gid, final)
		if err != nil {
			t.Fatal(err)
		}
		return n, b
	}
	if n, _ := pass(false); n != 3 {
		t.Fatalf("pre-copy sent %d files, want 3", n)
	}
	// Between the passes: a write, a new upload, a deletion.
	db.Exec(`INSERT INTO t VALUES ('two')`)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(filepath.Join(src, "uploads", "b.png"), []byte("new"), 0o600)
	os.Remove(filepath.Join(src, "gone.txt"))
	if n, _ := pass(true); n != 2 { // the database and b.png; a.jpg is unchanged
		t.Fatalf("final pass sent %d files, want 2", n)
	}
	for _, p := range []string{"uploads/2026/a.jpg", "uploads/b.png", "app.db"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("%s missing: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "gone.txt")); err == nil {
		t.Error("a file deleted on the source survived the final pass")
	}
	if _, err := os.Stat(filepath.Join(dst, "app.db-wal")); err == nil {
		t.Error("a WAL file was copied")
	}
	got, err := sql.Open("sqlite", "file:"+filepath.Join(dst, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	var n int
	if err := got.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows on the target: %d %v", n, err)
	}
}

func TestUnpackRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"../x", "/etc/x", "a/../../x", "not-in-manifest"} {
		var buf bytes.Buffer
		listed := []string{"ok", name}
		if name == "not-in-manifest" {
			listed = []string{"ok"}
		}
		tarWriter(&buf, listed, name)
		if _, _, err := UnpackDataDir(&buf, t.TempDir(), os.Getuid(), os.Getgid(), false); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// tarWriter writes a data stream whose manifest lists listed and which
// holds one file called name.
func tarWriter(buf *bytes.Buffer, listed []string, name string) *tar.Writer {
	tw := tar.NewWriter(buf)
	mb, _ := json.Marshal(listed)
	tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg})
	tw.Write(mb)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	return tw
}
