package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/s3"
	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "goboat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, err := secrets.LoadOrCreate(filepath.Join(root, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Layout: layout.Layout{Root: root}, Store: st, Box: box, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	data := s.Layout.DataDir("web")
	os.MkdirAll(data, 0o700)
	app, err := sql.Open("sqlite", "file:"+filepath.Join(data, "app.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	if _, err := app.Exec(`CREATE TABLE visits (at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	return s, app
}

// A scheduled backup is skipped while the databases don't change; a write
// makes the next one run. Manual and pre-deploy backups always run.
func TestScheduledBackupSkipsUnchanged(t *testing.T) {
	ctx := context.Background()
	s, app := newTestService(t)
	first, err := s.Run(ctx, "web", KindScheduled, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == "" {
		t.Fatal("no fingerprint stored")
	}
	for i := 0; i < 3; i++ {
		b, err := s.Run(ctx, "web", KindScheduled, true, nil)
		if !errors.Is(err, ErrUnchanged) || b.ID != first.ID {
			t.Fatalf("run %d: %v (backup #%d)", i, err, b.ID)
		}
	}
	if c, ok := s.LastCheck("web"); !ok || !c.Unchanged || !c.Since.Equal(first.CreatedAt) {
		t.Fatalf("check %+v", c)
	}
	if _, err := s.Run(ctx, "web", KindManual, true, nil); err != nil {
		t.Fatalf("manual backup of unchanged databases: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // a different mtime even on coarse clocks
	if _, err := app.Exec(`INSERT INTO visits VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	b, err := s.Run(ctx, "web", KindScheduled, true, nil)
	if err != nil {
		t.Fatalf("after a write: %v", err)
	}
	if b.ID == first.ID {
		t.Fatal("no new backup after a write")
	}
	if c, _ := s.LastCheck("web"); c.Unchanged {
		t.Fatalf("check says unchanged after a write: %+v", c)
	}
	if _, err := s.Run(ctx, "web", KindScheduled, true, nil); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("taking a backup must not count as a change: %v", err)
	}
}

// While uploads fail, local copies are kept for the retry: the newest
// KeepUnuploaded, then one per hour.
func TestPruneLocalThinsUnuploaded(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestService(t)
	cfg, _ := json.Marshal(s3.Config{Endpoint: "http://127.0.0.1:1", Bucket: "b", AccessKey: "a", SecretKey: "s", Region: "auto"})
	sealed, _ := s.Box.Seal(cfg, purposeS3)
	s.Store.SetSetting(ctx, SettingS3, sealed)

	dir := s.localDir("web")
	os.MkdirAll(dir, 0o700)
	// 4 hours of 10-minute backups (24), newest last, none uploaded.
	base := time.Now().Truncate(time.Hour).Add(-3 * time.Hour)
	for i := 0; i < 24; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		p := filepath.Join(dir, fmt.Sprintf("%s-scheduled-%d.tar.zst", at.UTC().Format("20060102T150405Z"), i))
		os.WriteFile(p, []byte("x"), 0o600)
		s.Store.Writer().Exec(`INSERT INTO backups (app, kind, status, local_path, created_at) VALUES ('web', 'scheduled', 'ok', ?, ?)`, p, at.Unix())
	}
	s.pruneLocal("web")
	left, _ := os.ReadDir(dir)
	// The newest 6 are in the last hour; the 3 older hours keep one each.
	if want := KeepUnuploaded + 3; len(left) != want {
		t.Fatalf("%d local copies kept, want %d", len(left), want)
	}
	var rows int
	s.Store.Reader().QueryRow(`SELECT count(*) FROM backups WHERE app = 'web'`).Scan(&rows)
	if rows != len(left) {
		t.Fatalf("%d rows for %d files: rows without any copy must be forgotten", rows, len(left))
	}
}

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.db"), []byte("one"), 0o600)
	f1 := Fingerprint(dir, []string{"a.db"})
	if f1 != Fingerprint(dir, []string{"a.db"}) {
		t.Fatal("not stable")
	}
	os.WriteFile(filepath.Join(dir, "a.db-wal"), []byte("w"), 0o600)
	if Fingerprint(dir, []string{"a.db"}) == f1 {
		t.Fatal("a new WAL is not a change")
	}
	if Fingerprint(dir, []string{"a.db", "b.db"}) == Fingerprint(dir, []string{"a.db"}) {
		t.Fatal("a new database is not a change")
	}
}
