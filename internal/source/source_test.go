package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type tarEntry struct {
	typ        byte // d, f, l
	name, link string
}

// parseEntries turns "f a/b\nl a/c ../x\nd a/d" into entries.
func parseEntries(spec string) []tarEntry {
	var es []tarEntry
	for _, line := range strings.Split(spec, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 1 {
			continue
		}
		e := tarEntry{typ: f[0][0], name: f[1]}
		if len(f) > 2 {
			e.link = f[2]
		}
		es = append(es, e)
	}
	return es
}

func writeTarGz(t testing.TB, path string, es []tarEntry) { writeTarGzPrefix(t, path, "./", es) }

func writeTarGzPrefix(t testing.TB, path, prefix string, es []tarEntry) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range es {
		h := &tar.Header{Name: prefix + e.name, Mode: 0o755}
		switch e.typ {
		case 'd':
			h.Typeflag = tar.TypeDir
		case 'l':
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, 2
		}
		if err := tw.WriteHeader(h); err != nil {
			continue
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte("hi"))
		}
	}
	tw.Close()
	zw.Close()
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// unpack runs Unpack with dst nested three levels deep and checks that
// nothing was written outside dst.
func unpack(t testing.TB, es []tarEntry) (string, error) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	os.MkdirAll(deep, 0o755)
	archive := filepath.Join(root, "app.tar.gz")
	writeTarGz(t, archive, es)
	dst := filepath.Join(deep, "dst")
	err := Unpack(archive, dst, Limits{MaxBytes: 1 << 20, MaxEntries: 100})
	want := map[string][]string{root: {"a", "app.tar.gz"}, filepath.Join(root, "a"): {"b"},
		filepath.Join(root, "a", "b"): {"c"}, deep: {"dst"}}
	for dir, names := range want {
		got, _ := os.ReadDir(dir)
		var have []string
		for _, g := range got {
			have = append(have, g.Name())
		}
		if strings.Join(have, ",") != strings.Join(names, ",") {
			t.Fatalf("extraction wrote outside dst: %s has %v (entries %+v)", dir, have, es)
		}
	}
	return dst, err
}

func TestUnpack(t *testing.T) {
	dst, err := unpack(t, parseEntries("d bin\nf bin/app\nf goboat.toml\nl static/link ../bin/app\nf templates/x.html"))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "static", "link")); err != nil || string(b) != "hi" {
		t.Fatalf("link: %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dst, "bin", "app")); st.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestUnpackRejects(t *testing.T) {
	for name, spec := range map[string]string{
		"dotdot":          "f ../evil",
		"absolute link":   "l x /etc/passwd",
		"escaping link":   "l x ../../y",
		"through symlink": "d a\nl a/b ..\nl a/b/c ..\nf a/b/c/evil",
		"file via link":   "l s .\nf s/../../evil",
		"link to dir":     "d real\nl sub real\nf sub/x",
		"dir over link":   "l d .\nd d",
		"duplicate file":  "f x\nf x",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := unpack(t, parseEntries(spec)); err == nil {
				t.Fatalf("accepted %q", spec)
			}
		})
	}
}

func TestUnpackLimits(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.tar.gz")
	writeTarGz(t, a, parseEntries("f a\nf b"))
	if err := Unpack(a, filepath.Join(dir, "d"), Limits{MaxBytes: 3, MaxEntries: 10}); err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("size limit: %v", err)
	}
	if err := Unpack(a, filepath.Join(dir, "d2"), Limits{MaxBytes: 1 << 20, MaxEntries: 1}); err == nil || !strings.Contains(err.Error(), "more than 1 entries") {
		t.Fatalf("entry limit: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "plain"), []byte("not gzip"), 0o600)
	if err := Unpack(filepath.Join(dir, "plain"), filepath.Join(dir, "e"), SourceLimits); err == nil {
		t.Fatal("non-gzip accepted")
	}
}

// GitHub's tarballs put everything in "<owner>-<repo>-<sha>/"; StripTop
// removes it and refuses entries outside it.
func TestUnpackStripTop(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.tar.gz")
	writeTarGzPrefix(t, a, "me-web-3f9c2a1/", parseEntries("d cmd\nf cmd/main.go\nf goboat.toml"))
	dst := filepath.Join(dir, "src")
	if err := Unpack(a, dst, SourceLimits); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"cmd/main.go", "goboat.toml"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, n := range []string{"top/a", "other/b"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Typeflag: tar.TypeReg, Size: 1})
		tw.Write([]byte("x"))
	}
	tw.Close()
	zw.Close()
	b := filepath.Join(dir, "b.tar.gz")
	os.WriteFile(b, buf.Bytes(), 0o600)
	if err := Unpack(b, filepath.Join(dir, "src2"), SourceLimits); err == nil || !strings.Contains(err.Error(), "must be inside top/") {
		t.Fatalf("two top-level dirs: %v", err)
	}
}

// FuzzUnpack builds archives from fuzzed tarEntry lists: extraction may fail,
// but must never write outside its destination.
func FuzzUnpack(f *testing.F) {
	for _, s := range []string{
		"d bin\nf bin/app",
		"d a\nl a/b ..\nl a/b/c ..\nf a/b/c/evil",
		"l x y\nl y ..\nf x/z",
		"l a .\nl a/b ..\nd a/b/c\nf a/b/c/d",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		es := parseEntries(spec)
		if len(es) > 12 {
			return
		}
		unpack(t, es)
	})
}

func TestCheckBinary(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable() // the test binary: an ELF for this machine
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(self)
	os.WriteFile(filepath.Join(root, "app"), b, 0o755)
	os.WriteFile(filepath.Join(root, "noexec"), b, 0o644)
	os.WriteFile(filepath.Join(root, "script"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("/bin/sh", filepath.Join(root, "link"))
	if err := CheckBinary(filepath.Join(root, "app"), runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	for rel, want := range map[string]string{"missing": "no binary", "noexec": "not executable",
		"script": "not a Linux (ELF) binary", "link": "not a regular file"} {
		if err := CheckBinary(filepath.Join(root, rel), runtime.GOARCH); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", rel, err, want)
		}
	}
	if err := CheckBinary(filepath.Join(root, "app"), other); err == nil || !strings.Contains(err.Error(), "but this server is "+other) {
		t.Errorf("wrong arch: %v", err)
	}
}
