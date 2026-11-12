// Package layout defines the on-disk layout under the data root
// (docs/architecture.md) and creates per-app directories with the right
// ownership and modes.
package layout

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/im-anuvaad/goboat/internal/users"
)

// Fixed locations. goboat has no config file (docs/architecture.md).
const (
	DataRoot  = "/var/lib/goboat"
	ConfigDir = "/etc/goboat"
	MasterKey = "/etc/goboat/master.key"
	// GoRoot is the Go toolchain goboat installs and builds with
	// (docs/architecture.md §4). A system Go elsewhere is never used.
	GoRoot = "/usr/local/lib/goboat/go"
)

// Layout resolves paths under the data root (DataRoot).
type Layout struct {
	Root string
	// UID and GID own the directories that are not the app user's
	// (EnsureApp). The zero value is root, as in production; tests that
	// run unprivileged set their own.
	UID, GID int
}

// Default is the layout under DataRoot.
func Default() Layout { return Layout{Root: DataRoot} }

// DBPath is goboat.db.
func (l Layout) DBPath() string { return filepath.Join(l.Root, "goboat.db") }

// SetupDir holds the self-signed certificate of the setup address.
func (l Layout) SetupDir() string { return filepath.Join(l.Root, "setup") }

// EnsureBase creates /etc/goboat (0700) and the data root (0711: app users
// can traverse to their own directories without listing anything).
func EnsureBase(l Layout) error {
	if err := os.MkdirAll(ConfigDir, 0o700); err != nil {
		return fmt.Errorf("layout: create %s: %w", ConfigDir, err)
	}
	if err := os.MkdirAll(l.Root, 0o711); err != nil {
		return fmt.Errorf("layout: create %s: %w", l.Root, err)
	}
	return os.Chmod(l.Root, 0o711)
}

// AppsDir is <root>/apps.
func (l Layout) AppsDir() string { return filepath.Join(l.Root, "apps") }

// AppDir is <root>/apps/<app>.
func (l Layout) AppDir(app string) string { return filepath.Join(l.AppsDir(), app) }

// BuildsDir is <root>/apps/<app>/builds: the kept builds.
func (l Layout) BuildsDir(app string) string { return filepath.Join(l.AppDir(app), "builds") }

// BuildDir is <root>/apps/<app>/builds/<id>: src/ (the source tree at the
// commit, the app's working directory) and app (the binary).
func (l Layout) BuildDir(app, id string) string { return filepath.Join(l.BuildsDir(app), id) }

// BuildSrc is the source tree of a kept build.
func (l Layout) BuildSrc(app, id string) string { return filepath.Join(l.BuildDir(app, id), "src") }

// BuildBinary is the binary of a kept build.
func (l Layout) BuildBinary(app, id string) string { return filepath.Join(l.BuildDir(app, id), "app") }

// CurrentLink is <root>/apps/<app>/current (symlink to the live build).
func (l Layout) CurrentLink(app string) string { return filepath.Join(l.AppDir(app), "current") }

// DataDir is the app's DATA_DIR.
func (l Layout) DataDir(app string) string { return filepath.Join(l.AppDir(app), "data") }

// TmpDir is the app's TMPDIR.
func (l Layout) TmpDir(app string) string { return filepath.Join(l.AppDir(app), "tmp") }

// LogsDir holds the app's runtime logs.
func (l Layout) LogsDir(app string) string { return filepath.Join(l.AppDir(app), "logs") }

// AppLog is the active runtime log file.
func (l Layout) AppLog(app string) string { return filepath.Join(l.LogsDir(app), "app.log") }

// DeployLogsDir holds the per-deployment logs (build output included).
func (l Layout) DeployLogsDir(app string) string { return filepath.Join(l.LogsDir(app), "deploys") }

// DeployLog is the log file of one deployment.
func (l Layout) DeployLog(app string, deployID int64) string {
	return filepath.Join(l.DeployLogsDir(app), fmt.Sprintf("%d.log", deployID))
}

// WorkRoot is <root>/work: build workspaces, emptied at start.
func (l Layout) WorkRoot() string { return filepath.Join(l.Root, "work") }

// WorkDir is the workspace of one build: <root>/work/<app>/<deployment id>.
func (l Layout) WorkDir(app string, deployID int64) string {
	return filepath.Join(l.WorkRoot(), app, fmt.Sprintf("%d", deployID))
}

// CacheDir is <root>/cache: the Go module and build caches shared by all
// builds (owned by the build user).
func (l Layout) CacheDir() string { return filepath.Join(l.Root, "cache") }

type dir struct {
	path  string
	mode  os.FileMode
	owned bool // owned by the app user instead of root
	group bool // root-owned, group = the app user's group
}

func (l Layout) appDirs(app string) []dir {
	return []dir{
		{l.AppsDir(), 0o711, false, false},
		{l.AppDir(app), 0o711, false, false},
		// Only the app (and root) may enter its builds: they hold its
		// source code.
		{l.BuildsDir(app), 0o750, false, true},
		{l.DataDir(app), 0o700, true, false},
		{l.TmpDir(app), 0o700, true, false},
		{l.LogsDir(app), 0o700, false, false},
		{l.DeployLogsDir(app), 0o700, false, false},
	}
}

// EnsureApp creates (or repairs the ownership and modes of) all directories
// for app. Only the directories themselves are fixed, not their contents.
func (l Layout) EnsureApp(app string, u users.User) error {
	for _, d := range l.appDirs(app) {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("layout: create %s: %w", d.path, err)
		}
		uid, gid := l.UID, l.GID
		if d.owned {
			uid, gid = int(u.UID), int(u.GID)
		}
		if d.group {
			gid = int(u.GID)
		}
		if err := os.Lchown(d.path, uid, gid); err != nil {
			return fmt.Errorf("layout: chown %s: %w", d.path, err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return fmt.Errorf("layout: chmod %s: %w", d.path, err)
		}
	}
	return nil
}

// WipeTmp empties the app's TMPDIR (done on deploy).
func (l Layout) WipeTmp(app string, u users.User) error {
	tmp := l.TmpDir(app)
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("layout: wipe %s: %w", tmp, err)
	}
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return fmt.Errorf("layout: create %s: %w", tmp, err)
	}
	if err := os.Chown(tmp, int(u.UID), int(u.GID)); err != nil {
		return fmt.Errorf("layout: chown %s: %w", tmp, err)
	}
	return nil
}
