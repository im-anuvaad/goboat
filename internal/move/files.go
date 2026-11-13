package move

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/im-anuvaad/goboat/internal/backup"
)

// FileInfo describes one file of a DATA_DIR.
type FileInfo struct {
	Path  string `json:"path"` // relative, slash-separated
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"` // unix nanoseconds
	Mode  uint32 `json:"mode"`
}

const manifestName = ".goboat-move-manifest.json"

// sqliteSidecar reports files SQLite keeps next to a database.
func sqliteSidecar(rel string, dbs map[string]bool) bool {
	for _, suf := range []string{"-wal", "-shm", "-journal"} {
		if base, ok := strings.CutSuffix(rel, suf); ok && dbs[base] {
			return true
		}
	}
	return false
}

// ListDataDir lists the regular files under dir. Symlinks and other
// special files are skipped (goboat never creates them; an app can't
// rely on them surviving a move).
func ListDataDir(dir string) ([]FileInfo, error) {
	var out []FileInfo
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, FileInfo{Path: filepath.ToSlash(rel), Size: fi.Size(), MTime: fi.ModTime().UnixNano(), Mode: uint32(fi.Mode().Perm())})
		return nil
	})
	return out, err
}

// PackDataDir writes a tar of dataDir to w: first a manifest of every file
// the target should end up with, then every SQLite database as a
// consistent snapshot (VACUUM INTO, taken in staging) and every other file
// whose size or mtime differs from have. SQLite's -wal/-shm/-journal files
// are left out: the snapshot holds everything.
func PackDataDir(ctx context.Context, dataDir, staging string, have []FileInfo, w io.Writer) (files int, bytes int64, err error) {
	dbList, err := backup.FindDatabases(dataDir)
	if err != nil {
		return 0, 0, err
	}
	dbs := map[string]bool{}
	for _, d := range dbList {
		dbs[filepath.ToSlash(d)] = true
	}
	all, err := ListDataDir(dataDir)
	if err != nil {
		return 0, 0, err
	}
	known := map[string]FileInfo{}
	for _, f := range have {
		known[f.Path] = f
	}
	var keep []string
	for _, f := range all {
		if !sqliteSidecar(f.Path, dbs) {
			keep = append(keep, f.Path)
		}
	}
	tw := tar.NewWriter(w)
	mb, _ := json.Marshal(keep)
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		return 0, 0, err
	}
	tw.Write(mb)

	work, err := os.MkdirTemp(staging, "move-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(work)
	add := func(src string, f FileInfo, db bool) error {
		fh, err := os.Open(src)
		if err != nil {
			return err
		}
		defer fh.Close()
		st, err := fh.Stat()
		if err != nil {
			return err
		}
		h := &tar.Header{Name: f.Path, Mode: int64(f.Mode), Size: st.Size(), ModTime: time.Unix(0, f.MTime), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
		if db {
			h.PAXRecords = map[string]string{"GOBOAT.sqlite": "1"}
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		n, err := io.Copy(tw, fh)
		files++
		bytes += n
		return err
	}
	for _, f := range all {
		if ctx.Err() != nil {
			return files, bytes, ctx.Err()
		}
		switch {
		case sqliteSidecar(f.Path, dbs):
		case dbs[f.Path]:
			snap := filepath.Join(work, fmt.Sprintf("db-%d", files))
			if err := backup.Snapshot(ctx, filepath.Join(dataDir, filepath.FromSlash(f.Path)), snap); err != nil {
				return files, bytes, fmt.Errorf("%s: %w", f.Path, err)
			}
			err = add(snap, f, true)
			os.Remove(snap)
			if err != nil {
				return files, bytes, err
			}
		case known[f.Path].Size == f.Size && known[f.Path].MTime == f.MTime && known[f.Path].Path != "":
		default:
			if err := add(filepath.Join(dataDir, filepath.FromSlash(f.Path)), f, false); err != nil {
				return files, bytes, err
			}
		}
	}
	return files, bytes, tw.Close()
}

// UnpackDataDir writes a PackDataDir stream into dataDir, owned by
// uid:gid. Every file is written to a temporary name and renamed into
// place; a database replaces its old -wal/-shm. With final, files that are
// not in the manifest are deleted, so dataDir ends up as on the source.
func UnpackDataDir(r io.Reader, dataDir string, uid, gid int, final bool) (files int, bytes int64, err error) {
	tr := tar.NewReader(r)
	var manifest map[string]bool
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, bytes, fmt.Errorf("reading the data: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			return files, bytes, fmt.Errorf("unexpected entry %q", h.Name)
		}
		if manifest == nil {
			if h.Name != manifestName {
				return files, bytes, errors.New("the data stream has no manifest")
			}
			var list []string
			if err := json.NewDecoder(io.LimitReader(tr, 64<<20)).Decode(&list); err != nil {
				return files, bytes, fmt.Errorf("manifest: %w", err)
			}
			manifest = map[string]bool{}
			for _, p := range list {
				manifest[p] = true
			}
			continue
		}
		rel := h.Name
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(rel)))
		if rel == "" || clean != rel || strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") || !manifest[rel] {
			return files, bytes, fmt.Errorf("unsafe path %q in the data stream", rel)
		}
		dst := filepath.Join(dataDir, filepath.FromSlash(rel))
		if err := mkdirOwned(dataDir, filepath.Dir(dst), uid, gid); err != nil {
			return files, bytes, err
		}
		tmp := dst + ".goboat-move.tmp"
		os.Remove(tmp)
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode)&0o777|0o600)
		if err != nil {
			return files, bytes, err
		}
		n, err := io.Copy(f, tr)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chown(tmp, uid, gid)
		}
		if err == nil {
			err = os.Chtimes(tmp, h.ModTime, h.ModTime)
		}
		if err == nil && h.PAXRecords["GOBOAT.sqlite"] == "1" {
			os.Remove(dst + "-wal")
			os.Remove(dst + "-shm")
			os.Remove(dst + "-journal")
		}
		if err == nil {
			err = os.Rename(tmp, dst)
		}
		if err != nil {
			os.Remove(tmp)
			return files, bytes, fmt.Errorf("%s: %w", rel, err)
		}
		files++
		bytes += n
	}
	if manifest == nil {
		return files, bytes, errors.New("the data stream is empty")
	}
	if final {
		local, err := ListDataDir(dataDir)
		if err != nil {
			return files, bytes, err
		}
		for _, f := range local {
			if !manifest[f.Path] {
				os.Remove(filepath.Join(dataDir, filepath.FromSlash(f.Path)))
			}
		}
	}
	return files, bytes, nil
}

// mkdirOwned creates dir (inside root) and its missing parents, owned by
// uid:gid, refusing to go through symlinks.
func mkdirOwned(root, dir string, uid, gid int) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is outside %s", dir, root)
	}
	cur := root
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		st, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(cur, 0o700); err != nil {
				return err
			}
			if err := os.Chown(cur, uid, gid); err != nil {
				return err
			}
		case err != nil:
			return err
		case !st.IsDir():
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}
