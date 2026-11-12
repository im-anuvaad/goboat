// Package builder builds an app's source tree with the Go toolchain on the
// server (docs/architecture.md §7.3): as the unprivileged build user, in
// its own cgroup with a memory limit that may use swap, a low CPU and I/O
// weight next to the running apps, and a time limit.
package builder

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/im-anuvaad/goboat/internal/cgroup"
	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/logs"
	"github.com/im-anuvaad/goboat/internal/users"
)

// Limits of one build.
const (
	MemoryMax = 1536 << 20 // plus swap
	Timeout   = 15 * time.Minute
	CPUWeight = 20 // apps have the default 100
	IOWeight  = 20
	PidsMax   = 1024
	// ModCacheMax: above it the module cache is emptied before a build
	// (the build cache trims itself).
	ModCacheMax = 5 << 30
)

// Builder runs builds. Builds are run one at a time by the deployer.
type Builder struct {
	Layout  layout.Layout
	Cgroups *cgroup.Manager
	GoRoot  string // default layout.GoRoot
	Log     *slog.Logger
}

func (b *Builder) goRoot() string {
	if b.GoRoot != "" {
		return b.GoRoot
	}
	return layout.GoRoot
}

func (b *Builder) cache(sub string) string { return filepath.Join(b.Layout.CacheDir(), sub) }

// Init creates the caches owned by the build user and empties the
// workspaces of builds interrupted by a restart.
func (b *Builder) Init() error {
	u, err := users.EnsureBuild()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(b.Layout.WorkRoot()); err != nil {
		return fmt.Errorf("builder: empty %s: %w", b.Layout.WorkRoot(), err)
	}
	if err := mkdir(b.Layout.WorkRoot(), 0o711, 0, 0); err != nil {
		return err
	}
	for _, d := range []string{b.Layout.CacheDir(), b.cache("mod"), b.cache("build"), b.cache("home")} {
		if err := mkdir(d, 0o700, int(u.UID), int(u.GID)); err != nil {
			return err
		}
	}
	return nil
}

func mkdir(p string, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(p, mode); err != nil {
		return fmt.Errorf("builder: create %s: %w", p, err)
	}
	if err := os.Chown(p, uid, gid); err != nil {
		return fmt.Errorf("builder: chown %s: %w", p, err)
	}
	return os.Chmod(p, mode)
}

// Workspace is the directory of one build: the downloaded archive and the
// source tree (written by goboat, read-only for the build) and the output
// binary and temporary files (written by the build user).
type Workspace struct {
	Dir     string
	Archive string // source.tar.gz
	Src     string // the unpacked source tree
	Binary  string // the build output
	Tmp     string
}

// Prepare creates an empty workspace for deployment id of app.
func (b *Builder) Prepare(app string, id int64) (Workspace, error) {
	u, err := users.LookupBuild()
	if err != nil {
		return Workspace{}, fmt.Errorf("the build user is missing (re-run the install command): %w", err)
	}
	dir := b.Layout.WorkDir(app, id)
	if err := mkdir(filepath.Dir(dir), 0o711, 0, 0); err != nil {
		return Workspace{}, err
	}
	os.RemoveAll(dir)
	if err := mkdir(dir, 0o700, int(u.UID), int(u.GID)); err != nil {
		return Workspace{}, err
	}
	ws := Workspace{
		Dir:     dir,
		Archive: filepath.Join(dir, "source.tar.gz"),
		Src:     filepath.Join(dir, "src"),
		Binary:  filepath.Join(dir, "app"),
		Tmp:     filepath.Join(dir, "tmp"),
	}
	if err := mkdir(ws.Tmp, 0o700, int(u.UID), int(u.GID)); err != nil {
		return Workspace{}, err
	}
	return ws, nil
}

// Remove deletes a workspace.
func (b *Builder) Remove(ws Workspace) {
	if ws.Dir != "" {
		os.RemoveAll(ws.Dir)
	}
}

// Result describes a successful build.
type Result struct {
	GoVersion  string // of the toolchain that built it, e.g. go1.27.4
	Took       time.Duration
	PeakMemory int64
	PeakSwap   int64
}

// Build runs go build for pkg in ws.Src, writing ws.Binary. The build's
// output goes to lg.
func (b *Builder) Build(ctx context.Context, appName string, id int64, ws Workspace, pkg string, lg *logs.Log) (Result, error) {
	var res Result
	goBin := filepath.Join(b.goRoot(), "bin", "go")
	if _, err := os.Stat(goBin); err != nil {
		return res, fmt.Errorf("the Go toolchain is missing at %s; re-run the install command", b.goRoot())
	}
	u, err := users.LookupBuild()
	if err != nil {
		return res, fmt.Errorf("the build user is missing (re-run the install command): %w", err)
	}
	b.trimModCache(lg)

	g, err := b.Cgroups.Group(cgroup.Builds, fmt.Sprintf("%s-%d", appName, id))
	if err != nil {
		return res, err
	}
	defer func() {
		g.Kill()
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		g.WaitEmpty(wctx)
		cancel()
		g.Remove()
	}()
	mem := memoryLimit()
	if _, err := g.SetLimits(cgroup.Limits{MemoryMax: mem, CPUWeight: CPUWeight, PidsMax: PidsMax}); err != nil {
		return res, err
	}
	if _, err := g.SetLimits(cgroup.Limits{IOWeight: IOWeight}); err != nil && b.Log != nil {
		b.Log.Warn("setting the build I/O weight failed; builds compete equally for the disk", "err", err)
	}

	fd, err := g.OpenFD()
	if err != nil {
		return res, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		unix.Close(fd)
		return res, err
	}
	args := []string{"build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", ws.Binary, pkg}
	lg.Writef("go %s", joinArgs(args))
	cmd := exec.Command(goBin, args...)
	cmd.Dir = ws.Src
	cmd.Env = b.environ(ws)
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: []uint32{}},
		Setpgid:     true,
		Pdeathsig:   syscall.SIGKILL,
		UseCgroupFD: true,
		CgroupFD:    fd,
	}
	start := time.Now()
	err = cmd.Start()
	unix.Close(fd)
	pw.Close()
	if err != nil {
		pr.Close()
		return res, fmt.Errorf("starting go build: %w", err)
	}
	captured := make(chan struct{})
	go func() { lg.Capture(logs.Stdout, pr); pr.Close(); close(captured) }()

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	timer := time.NewTimer(Timeout)
	defer timer.Stop()
	var why error
	select {
	case err = <-waited:
	case <-timer.C:
		why = fmt.Errorf("the build took longer than %s and was stopped", Timeout)
		g.Kill()
		err = <-waited
	case <-ctx.Done():
		why = fmt.Errorf("the build was cancelled: %w", ctx.Err())
		g.Kill()
		err = <-waited
	}
	g.Kill() // anything the go command left behind
	<-captured
	res.Took = time.Since(start)
	res.PeakMemory, res.PeakSwap = g.Peaks()
	st, _ := g.Stats()
	lg.Writef("build took %s, peak memory %s, peak swap %s", res.Took.Round(100*time.Millisecond), mb(res.PeakMemory), mb(res.PeakSwap))
	switch {
	case why != nil:
		return res, why
	case err != nil && st.OOMKills > 0:
		return res, fmt.Errorf("the build ran out of memory (limit %s plus swap) and was killed; stop apps you don't need while building, or use a server with more memory", mb(mem))
	case err != nil:
		return res, fmt.Errorf("go build failed (%v); the deploy log shows the compiler output", err)
	}
	if info, err := buildinfo.ReadFile(ws.Binary); err == nil {
		res.GoVersion = info.GoVersion
	}
	return res, nil
}

func (b *Builder) environ(ws Workspace) []string {
	home := b.cache("home")
	return []string{
		"PATH=" + filepath.Join(b.goRoot(), "bin") + ":/usr/local/bin:/usr/bin:/bin",
		"LANG=C.UTF-8",
		"HOME=" + home,
		"GOPATH=" + filepath.Join(home, "go"),
		"GOMODCACHE=" + b.cache("mod"),
		"GOCACHE=" + b.cache("build"),
		"TMPDIR=" + ws.Tmp,
		"GOENV=off",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=auto", // go.mod may ask for a newer Go; it is downloaded and verified
		"CGO_ENABLED=0",
		"GOFLAGS=",
	}
}

// memoryLimit is MemoryMax, or 3/4 of the RAM on smaller machines.
func memoryLimit() int64 {
	var si unix.Sysinfo_t
	if unix.Sysinfo(&si) == nil {
		ram := int64(si.Totalram) * int64(si.Unit)
		if ram > 0 && ram*3/4 < MemoryMax {
			return ram * 3 / 4
		}
	}
	return MemoryMax
}

// trimModCache empties the module cache when it has grown above
// ModCacheMax; the next builds download what they need again.
func (b *Builder) trimModCache(lg *logs.Log) {
	dir := b.cache("mod")
	var size int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	if size <= ModCacheMax {
		return
	}
	lg.Writef("the Go module cache is %s (above %s); emptying it, so this build downloads its modules again", mb(size), mb(ModCacheMax))
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lg.Writef("warning: emptying the module cache: %v", err)
	}
	if u, err := users.LookupBuild(); err == nil {
		mkdir(dir, 0o700, int(u.UID), int(u.GID))
	}
}

func mb(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%d MB", n>>20)
}

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		if s == "" || containsSpace(s) {
			s = fmt.Sprintf("%q", s)
		}
		out += s
	}
	return out
}

func containsSpace(s string) bool {
	for _, r := range s {
		if r == ' ' || r == '\t' {
			return true
		}
	}
	return false
}
