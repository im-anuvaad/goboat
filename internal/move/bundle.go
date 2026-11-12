package move

import (
	"archive/tar"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/im-anuvaad/goboat/internal/layout"
)

// Settings keys of the move.
const (
	SettingSource   = "move_state"    // on the old server, sealed
	SettingIncoming = "move_incoming" // on the new server, sealed
	SettingMovedTo  = "moved_to"      // on the old server after the move
	purposeSource   = "settings:move_state"
	purposeIncoming = "settings:move_incoming"
)

// bundle entries
const (
	bundleDB  = "goboat.db"
	bundleKey = "master.key"
)

// MakeBundle writes the old server's state for the new one to w as a tar:
// a snapshot of goboat.db without what belongs to this server (sessions,
// the one-time password, metrics, local backup copies, kept builds, the
// move itself), master.key (the database's sealed values need it), the
// AOP CA and client certificate, and the Origin CA certificates. Reusing
// these means Cloudflare needs no new certificate and no AOP rollout.
func MakeBundle(ctx context.Context, lay layout.Layout, masterKey string, db *sql.DB, w io.Writer) error {
	work, err := os.MkdirTemp(filepath.Join(lay.Root, "backups", "staging"), "move-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	snap := filepath.Join(work, bundleDB)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, snap); err != nil {
		return fmt.Errorf("snapshot of goboat.db: %w", err)
	}
	if err := trimForMove(ctx, snap); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	if err := addFile(tw, bundleDB, snap); err != nil {
		return err
	}
	if err := addFile(tw, bundleKey, masterKey); err != nil {
		return err
	}
	for _, dir := range []string{"aop", "certs"} {
		root := filepath.Join(lay.Root, dir)
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return filepath.SkipAll
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(lay.Root, p)
			return addFile(tw, filepath.ToSlash(rel), p)
		})
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// trimForMove removes what must not move from a goboat.db copy.
func trimForMove(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, q := range []string{
		`DELETE FROM sessions`,
		`DELETE FROM settings WHERE key IN ('setup_password', '` + SettingSource + `', '` + SettingMovedTo + `', '` + SettingIncoming + `')`,
		`DELETE FROM metrics_1m`,
		`DELETE FROM builds`,
		`UPDATE backups SET local_path = ''`,
		`DELETE FROM backups WHERE object_key = ''`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("preparing goboat.db for the move: %w", err)
		}
	}
	return nil
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// InstallBundle puts a bundle in place on the new server, which must be
// stopped and empty: goboat.db, master.key, aop/ and certs/ are replaced.
func InstallBundle(r io.Reader, lay layout.Layout, masterKey string) error {
	stage := filepath.Join(lay.Root, "move")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	tr := tar.NewReader(r)
	got := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading the settings: %w", err)
		}
		name := h.Name
		ok := name == bundleDB || name == bundleKey ||
			((strings.HasPrefix(name, "aop/") || strings.HasPrefix(name, "certs/")) && filepath.ToSlash(filepath.Clean(name)) == name && !strings.Contains(name, ".."))
		if h.Typeflag != tar.TypeReg || !ok {
			return fmt.Errorf("unexpected entry %q in the settings", name)
		}
		dst := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, io.LimitReader(tr, 4<<30))
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		got[name] = true
	}
	if !got[bundleDB] || !got[bundleKey] {
		return errors.New("the settings from the old server are incomplete")
	}
	// Into place. The database last, so an interrupted install leaves no
	// database that the key doesn't open.
	for _, p := range []string{lay.DBPath(), lay.DBPath() + "-wal", lay.DBPath() + "-shm"} {
		os.Remove(p)
	}
	for _, dir := range []string{"aop", "certs"} {
		dst := filepath.Join(lay.Root, dir)
		os.RemoveAll(dst)
		if _, err := os.Stat(filepath.Join(stage, dir)); err == nil {
			if err := os.Rename(filepath.Join(stage, dir), dst); err != nil {
				return err
			}
		}
	}
	if err := replaceFile(filepath.Join(stage, bundleKey), masterKey); err != nil {
		return err
	}
	return replaceFile(filepath.Join(stage, bundleDB), lay.DBPath())
}

func replaceFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".move.tmp"
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chtimes(tmp, time.Now(), time.Now()); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
