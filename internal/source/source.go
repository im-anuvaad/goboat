// Package source unpacks the source archives goboat builds from (GitHub's
// tarball of a commit, docs/architecture.md §7.2) and the Go toolchain
// archive, safely, and checks that a built binary runs on this machine.
package source

import (
	"archive/tar"
	"compress/gzip"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Limits bound an archive.
type Limits struct {
	MaxBytes   int64 // total size of the regular files
	MaxEntries int
	// StripTop removes the single top-level directory every entry is in
	// (GitHub's "<owner>-<repo>-<sha>/", Go's "go/"). Entries outside it
	// are rejected.
	StripTop bool
}

// SourceLimits apply to an app's source archive (docs/app-contract.md §3).
var SourceLimits = Limits{MaxBytes: 500 << 20, MaxEntries: 50000, StripTop: true}

// MaxArchive is the largest source archive goboat downloads.
const MaxArchive = 100 << 20

// Unpack extracts the gzipped tarball archive into dst (created). It
// rejects absolute paths, "..", device files, hard links, symlinks that
// point outside dst, paths through symlinks, and archives above lim. Files
// get mode 0644, or 0755 if any exec bit is set; directories 0755.
func Unpack(archive, dst string, lim Limits) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip file: %w", err)
	}
	defer zr.Close()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	top := ""
	for n := 0; ; n++ {
		if n > lim.MaxEntries {
			return fmt.Errorf("archive has more than %d entries", lim.MaxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
			continue // metadata only (GitHub stores the commit id here)
		}
		name := strings.TrimPrefix(h.Name, "./")
		if lim.StripTop {
			first, rest, _ := strings.Cut(strings.TrimSuffix(name, "/"), "/")
			if top == "" {
				top = first
			}
			if first != top || first == "" || first == ".." {
				return fmt.Errorf("unexpected path %q in archive: everything must be inside %s/", h.Name, top)
			}
			name = rest
		}
		if name == "" || name == "." || name == "./" {
			continue
		}
		rel := filepath.Clean(name)
		if filepath.IsAbs(name) || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path %q in archive", h.Name)
		}
		target := filepath.Join(dst, rel)
		// A path through a symlink extracted earlier could escape dst
		// (a/b -> .., a/b/c -> .. resolves outside), so parents must be
		// real directories and files must not exist yet.
		if err := realParents(dst, rel); err != nil {
			return fmt.Errorf("unsafe path %q in archive: %w", h.Name, err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if st, err := os.Lstat(target); err == nil && !st.IsDir() {
				return fmt.Errorf("unsafe path %q in archive: exists and is not a directory", h.Name)
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if h.Size < 0 || total > lim.MaxBytes {
				return fmt.Errorf("archive expands to more than %d MB", lim.MaxBytes>>20)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			if _, err := io.CopyN(out, tr, h.Size); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			lt := h.Linkname
			resolved := filepath.Clean(filepath.Join(filepath.Dir(rel), lt))
			if filepath.IsAbs(lt) || resolved == ".." || strings.HasPrefix(resolved, "../") {
				return fmt.Errorf("symlink %q -> %q escapes the archive", h.Name, lt)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(lt, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry type %q for %q", h.Typeflag, h.Name)
		}
	}
}

// realParents checks that every existing parent of rel below dst is a
// directory, not a symlink.
func realParents(dst, rel string) error {
	cur := dst
	for _, p := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if p == "." || p == "" {
			continue
		}
		cur = filepath.Join(cur, p)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !st.IsDir() {
			return fmt.Errorf("%s is not a directory", strings.TrimPrefix(cur, dst+"/"))
		}
	}
	return nil
}

var machines = map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}

// CheckBinary checks that path is an executable Linux ELF binary for goarch.
func CheckBinary(path, goarch string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the build produced no binary: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("the build output %s is not a regular file", filepath.Base(path))
	}
	if st.Mode()&0o111 == 0 {
		return fmt.Errorf("the build output %s is not executable", filepath.Base(path))
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("the build output %s is not a Linux (ELF) binary", filepath.Base(path))
	}
	defer f.Close()
	want, ok := machines[goarch]
	if !ok {
		return fmt.Errorf("unsupported server architecture %s", goarch)
	}
	if f.Machine != want {
		return fmt.Errorf("the build output is for %s, but this server is %s", f.Machine, goarch)
	}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("the build output is not an executable (ELF type %s)", f.Type)
	}
	return nil
}
