package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/source"
)

// The Go toolchain (docs/architecture.md §4).
const (
	goIndexURL    = "https://go.dev/dl/?mode=json"
	goDownloadURL = "https://dl.google.com/go/"
	goMaxArchive  = 512 << 20
)

var goLimits = source.Limits{MaxBytes: 2 << 30, MaxEntries: 200000, StripTop: true}

type goRelease struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
	Files   []struct {
		Filename string `json:"filename"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		SHA256   string `json:"sha256"`
		Kind     string `json:"kind"`
	} `json:"files"`
}

// installGo installs (or updates to) the latest stable Go into
// layout.GoRoot, verified against the checksum go.dev publishes. A Go
// installed elsewhere on the server is not touched. When go.dev can't be
// reached, an installed toolchain is kept.
func installGo(ctx context.Context, out io.Writer) error {
	have := installedGo()
	file, version, sum, err := latestGo(ctx)
	if err != nil {
		if have != "" {
			fmt.Fprintf(out, "warning: could not check for a newer Go (%v); keeping %s\n", err, have)
			return nil
		}
		return fmt.Errorf("installing Go: %w", err)
	}
	if have == version {
		fmt.Fprintf(out, "Go %s is installed\n", strings.TrimPrefix(version, "go"))
		return nil
	}
	fmt.Fprintf(out, "Installing Go %s (for builds) into %s\n", strings.TrimPrefix(version, "go"), layout.GoRoot)

	parent := filepath.Dir(layout.GoRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	archive := filepath.Join(parent, "go-download.tar.gz")
	os.Remove(archive)
	defer os.Remove(archive)
	if err := download(ctx, goDownloadURL+file, archive, sum); err != nil {
		return fmt.Errorf("downloading %s: %w", file, err)
	}
	next := layout.GoRoot + ".new"
	os.RemoveAll(next)
	if err := source.Unpack(archive, next, goLimits); err != nil {
		os.RemoveAll(next)
		return fmt.Errorf("unpacking %s: %w", file, err)
	}
	if _, err := os.Stat(filepath.Join(next, "bin", "go")); err != nil {
		os.RemoveAll(next)
		return fmt.Errorf("%s has no bin/go", file)
	}
	old := layout.GoRoot + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(layout.GoRoot); err == nil {
		if err := os.Rename(layout.GoRoot, old); err != nil {
			return err
		}
	}
	if err := os.Rename(next, layout.GoRoot); err != nil {
		os.Rename(old, layout.GoRoot)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// installedGo is the version of the toolchain in layout.GoRoot ("" if none).
func installedGo() string {
	b, err := os.ReadFile(filepath.Join(layout.GoRoot, "VERSION"))
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(first)
}

func latestGo(ctx context.Context) (file, version, sum string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, goIndexURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("%s: HTTP %d", goIndexURL, resp.StatusCode)
	}
	var rels []goRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		return "", "", "", fmt.Errorf("%s: %w", goIndexURL, err)
	}
	for _, r := range rels {
		if !r.Stable {
			continue
		}
		for _, f := range r.Files {
			if f.OS == "linux" && f.Arch == runtime.GOARCH && f.Kind == "archive" && len(f.SHA256) == 64 &&
				!strings.ContainsAny(f.Filename, "/\\") {
				return f.Filename, r.Version, f.SHA256, nil
			}
		}
	}
	return "", "", "", fmt.Errorf("no stable Go for linux/%s in %s", runtime.GOARCH, goIndexURL)
}

// download fetches url into path and checks its SHA-256.
func download(ctx context.Context, url, path, wantSHA string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, goMaxArchive+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
	case n > goMaxArchive:
		err = errors.New("the download is unexpectedly large")
	case hex.EncodeToString(h.Sum(nil)) != wantSHA:
		err = fmt.Errorf("checksum mismatch: go.dev says %s, the download is %s", wantSHA, hex.EncodeToString(h.Sum(nil)))
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// Swap for builds (docs/architecture.md §4).
const (
	swapFile   = "/goboat.swap"
	swapSize   = 2 << 30
	sysctlConf = "/etc/sysctl.d/90-goboat.conf"
)

// ensureSwap creates a 2 GB swap file unless the server already has that
// much swap. Apps never swap (their cgroups forbid it); builds may.
func ensureSwap(out io.Writer) error {
	total, active, err := swaps()
	if err != nil {
		return err
	}
	if active[swapFile] {
		fmt.Fprintf(out, "Swap: %s is on (%s of swap in total)\n", swapFile, gib(total))
		return addFstab()
	}
	if total >= swapSize-(64<<20) {
		fmt.Fprintf(out, "Swap: the server has %s already; leaving it as it is\n", gib(total))
		return nil
	}
	fmt.Fprintf(out, "Creating a %s swap file at %s (builds may use it, apps never do)\n", gib(swapSize), swapFile)
	if err := makeSwap(true); err != nil {
		// Some filesystems refuse swap files with unwritten (fallocated)
		// blocks; write them out instead.
		if err := makeSwap(false); err != nil {
			os.Remove(swapFile)
			return fmt.Errorf("creating the swap file: %w", err)
		}
	}
	if err := addFstab(); err != nil {
		return err
	}
	if err := os.WriteFile(sysctlConf, []byte("# goboat: prefer dropping cache over swapping (builds may swap; apps cannot)\nvm.swappiness = 10\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile("/proc/sys/vm/swappiness", []byte("10"), 0o644)
}

func makeSwap(fast bool) error {
	os.Remove(swapFile)
	f, err := os.OpenFile(swapFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if fast {
		err = unix.Fallocate(int(f.Fd()), 0, 0, swapSize)
	} else {
		buf := make([]byte, 1<<20)
		for i := 0; i < swapSize>>20 && err == nil; i++ {
			_, err = f.Write(buf)
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"mkswap", swapFile}, {"swapon", swapFile}} {
		if b, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
		}
	}
	return nil
}

func addFstab() error {
	b, err := os.ReadFile("/etc/fstab")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == swapFile {
			return nil
		}
	}
	f, err := os.OpenFile("/etc/fstab", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	prefix := ""
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		prefix = "\n"
	}
	_, err = fmt.Fprintf(f, "%s%s none swap sw 0 0\n", prefix, swapFile)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// swaps reads /proc/swaps: the total size and the active swap areas.
func swaps() (int64, map[string]bool, error) {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	var total int64
	active := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 3 {
			continue
		}
		kb, _ := strconv.ParseInt(fs[2], 10, 64)
		total += kb << 10
		active[fs[0]] = true
	}
	return total, active, sc.Err()
}

func gib(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }
