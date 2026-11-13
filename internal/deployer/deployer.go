// Package deployer implements the deploy pipeline (docs/architecture.md
// §7): source archive of a commit → safe unpack → goboat.toml → go build in
// the build sandbox → ELF check → stop old → pre-deploy backup → switch →
// start + health check, going back to the previous build when the new one
// is unhealthy. The running app keeps serving while its new build is made.
//
// All builds, deploys and rollbacks on the host run one at a time from a
// single queue.
package deployer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/im-anuvaad/goboat/internal/app"
	"github.com/im-anuvaad/goboat/internal/builder"
	"github.com/im-anuvaad/goboat/internal/github"
	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/logs"
	"github.com/im-anuvaad/goboat/internal/manifest"
	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/source"
	"github.com/im-anuvaad/goboat/internal/store"
	"github.com/im-anuvaad/goboat/internal/supervisor"
	"github.com/im-anuvaad/goboat/internal/users"
)

// Retention.
const (
	KeepBuilds     = 3
	KeepDeployLogs = 20
)

// Settings keys.
const (
	SettingGitHubToken = "github_token"
	purposeGitHubToken = "settings:github_token"
)

const (
	commitsTTL = time.Minute
	// ListCommits is how many commits of the branch the app page offers.
	ListCommits = 10
)

// AppConfig is the deploy configuration of one app.
type AppConfig struct {
	Base   app.Spec // name, domain, port, env, limits (no build fields)
	Repo   github.Repo
	Branch string // "" = the repository's default branch
}

// Deps are the deployer's collaborators.
type Deps struct {
	Layout     layout.Layout
	Store      *store.Store
	Secrets    *secrets.Box
	Supervisor *supervisor.Supervisor
	Builder    *builder.Builder
	Log        *slog.Logger
	// PreDeploy runs after the old build stopped and before the switch
	// (the SQLite backup). A returned error aborts the deploy and restarts
	// the old build.
	PreDeploy func(ctx context.Context, app string, log *logs.Log) error
	// GitHubBase overrides the GitHub API (tests).
	GitHubBase string
}

// Deployer runs deployments.
type Deployer struct {
	Deps
	db  *store.Store
	log *slog.Logger

	mu        sync.Mutex
	apps      map[string]AppConfig
	pending   map[string]int64  // app -> queued or running deployment
	held      map[string]string // app -> reason (e.g. a restore) that blocks deployments
	deploying map[string]bool   // app is between "stop old" and "healthy/failed"
	commits   map[string]cachedCommits
	queued    []int64 // deployment ids in queue order, for "position in queue"

	queue  chan job
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type job struct {
	id     int64
	app    string
	kind   string
	sha    string // commit to build and deploy, or of the kept build to roll back to
	branch string
}

type cachedCommits struct {
	at     time.Time
	branch string
	list   []github.Commit
	err    error
}

// New creates a deployer and marks interrupted deployments as failed.
// Call Run to start the worker.
func New(ctx context.Context, d Deps) (*Deployer, error) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	dp := &Deployer{
		Deps:      d,
		db:        d.Store,
		log:       d.Log,
		apps:      map[string]AppConfig{},
		pending:   map[string]int64{},
		held:      map[string]string{},
		deploying: map[string]bool{},
		commits:   map[string]cachedCommits{},
		queue:     make(chan job, 64),
	}
	if n, err := dp.recoverInterrupted(ctx); err != nil {
		return nil, err
	} else if n > 0 {
		dp.log.Warn("marked deployments interrupted by a restart as failed", "count", n)
	}
	return dp, nil
}

func (d *Deployer) github(ctx context.Context) (*github.API, error) {
	token, err := d.GitHubToken(ctx)
	if err != nil {
		return nil, err
	}
	return &github.API{Token: token, Base: d.GitHubBase}, nil
}

// Register adds an app and its current build (if any) to the supervisor.
func (d *Deployer) Register(ctx context.Context, cfg AppConfig) (*supervisor.App, error) {
	spec := cfg.Base
	if cur := d.currentBuild(spec.Name); cur != "" {
		b, err := d.build(ctx, spec.Name, cur)
		if err != nil {
			d.log.Warn("current build has no record; the app needs a deploy", "app", spec.Name, "build", cur, "err", err)
		} else {
			spec = d.specFor(cfg, b)
		}
	}
	a, err := d.Supervisor.Add(spec)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.apps[spec.Name] = cfg
	d.mu.Unlock()
	return a, nil
}

func (d *Deployer) specFor(cfg AppConfig, b Build) app.Spec {
	s := cfg.Base
	s.BuildID = b.ID
	s.Branch = b.Branch
	s.Commit = b.GitSHA
	s.Dir = d.Layout.BuildSrc(s.Name, b.ID)
	s.Run = append([]string{d.Layout.BuildBinary(s.Name, b.ID)}, b.Args...)
	s.HealthPath = b.HealthPath
	return s
}

// currentBuild reads the `current` symlink ("" if none).
func (d *Deployer) currentBuild(appName string) string {
	t, err := os.Readlink(d.Layout.CurrentLink(appName))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// Deploying reports whether app is in the downtime window of a deploy
// (the router shows a 503 "deploying" page then).
func (d *Deployer) Deploying(appName string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deploying[appName]
}

// Deploy queues a build and deploy of commit sha of the app's branch.
func (d *Deployer) Deploy(ctx context.Context, appName, sha string) (int64, error) {
	sha = strings.ToLower(strings.TrimSpace(sha))
	if sha == "" {
		return 0, errors.New("choose a commit to deploy")
	}
	if !github.ValidSHA(sha) {
		return 0, fmt.Errorf("%q is not a full commit SHA", sha)
	}
	cfg, ok := d.Config(appName)
	if !ok {
		return 0, fmt.Errorf("unknown app %q", appName)
	}
	if a := d.Supervisor.Get(appName); a != nil && a.Spec().Commit == sha {
		return 0, fmt.Errorf("commit %s is already running (use Restart to restart it)", sha[:7])
	}
	return d.enqueue(ctx, appName, KindDeploy, sha, cfg.Branch)
}

// BranchOf returns the branch an app is deployed from, resolving "" to
// the repository's default branch.
func (d *Deployer) BranchOf(ctx context.Context, cfg AppConfig) (string, error) {
	if cfg.Branch != "" {
		return cfg.Branch, nil
	}
	api, err := d.github(ctx)
	if err != nil {
		return "", err
	}
	info, err := api.Repo(ctx, cfg.Repo)
	if err != nil {
		return "", err
	}
	return info.DefaultBranch, nil
}

// Commits lists the newest commits of an app's branch, cached for a
// minute (the app page refreshes every few seconds).
func (d *Deployer) Commits(ctx context.Context, appName string) (string, []github.Commit, error) {
	d.mu.Lock()
	cfg, ok := d.apps[appName]
	c, cached := d.commits[appName]
	d.mu.Unlock()
	if !ok {
		return "", nil, fmt.Errorf("unknown app %q", appName)
	}
	if cached && time.Since(c.at) < commitsTTL && (cfg.Branch == "" || c.branch == cfg.Branch) {
		return c.branch, c.list, c.err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	branch, err := d.BranchOf(cctx, cfg)
	var list []github.Commit
	if err == nil {
		var api *github.API
		if api, err = d.github(cctx); err == nil {
			list, err = api.Commits(cctx, cfg.Repo, branch, ListCommits)
		}
	}
	d.mu.Lock()
	d.commits[appName] = cachedCommits{at: time.Now(), branch: branch, list: list, err: err}
	d.mu.Unlock()
	return branch, list, err
}

// ForgetCommits drops the cached commit list of app ("" = every app).
func (d *Deployer) ForgetCommits(appName string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if appName == "" {
		d.commits = map[string]cachedCommits{}
	} else {
		delete(d.commits, appName)
	}
}

// Rollback queues a switch to a kept build (no rebuild).
func (d *Deployer) Rollback(ctx context.Context, appName, buildID string) (int64, error) {
	b, err := d.build(ctx, appName, buildID)
	if err != nil {
		return 0, err
	}
	if d.currentBuild(appName) == b.ID {
		return 0, fmt.Errorf("build %s is already current", b.Short())
	}
	return d.enqueue(ctx, appName, KindRollback, b.GitSHA, b.Branch)
}

func (d *Deployer) enqueue(ctx context.Context, appName, kind, sha, branch string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.apps[appName]; !ok {
		return 0, fmt.Errorf("unknown app %q", appName)
	}
	if id, ok := d.pending[appName]; ok {
		return 0, fmt.Errorf("deployment #%d of %s is still in progress", id, appName)
	}
	if why, ok := d.held[appName]; ok {
		return 0, fmt.Errorf("%s: %s is in progress; deploy when it has finished", appName, why)
	}
	if d.ctx == nil || d.ctx.Err() != nil {
		return 0, errors.New("deployer is not running")
	}
	id, err := d.insertDeployment(ctx, appName, kind, sha, branch)
	if err != nil {
		return 0, err
	}
	select {
	case d.queue <- job{id: id, app: appName, kind: kind, sha: sha, branch: branch}:
	default:
		d.finish(id, errors.New("deploy queue is full"))
		return 0, errors.New("deploy queue is full")
	}
	d.pending[appName] = id
	d.queued = append(d.queued, id)
	return id, nil
}

// QueuePosition is how many deployments run or wait before id (0 = it is
// running or done).
func (d *Deployer) QueuePosition(id int64) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, q := range d.queued {
		if q == id {
			return i
		}
	}
	return 0
}

// Run starts the worker. Stop cancels the running job and waits.
func (d *Deployer) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx, d.cancel = context.WithCancel(ctx)
	d.mu.Unlock()
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		for {
			select {
			case <-d.ctx.Done():
				for {
					select {
					case j := <-d.queue:
						d.finish(j.id, errors.New("cancelled: goboat is shutting down"))
					default:
						return
					}
				}
			case j := <-d.queue:
				d.runJob(j)
			}
		}
	}()
}

// Stop cancels the current job and waits for the worker to exit.
func (d *Deployer) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	d.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
}

func (d *Deployer) runJob(j job) {
	d.mu.Lock()
	cfg := d.apps[j.app]
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, j.app)
		delete(d.deploying, j.app)
		for i, q := range d.queued {
			if q == j.id {
				d.queued = append(d.queued[:i], d.queued[i+1:]...)
				break
			}
		}
		d.mu.Unlock()
	}()

	lg, err := logs.Open(d.Layout.DeployLog(j.app, j.id), logs.Options{MaxSize: 64 << 20, Keep: 1, RingSize: 64})
	if err != nil {
		d.finish(j.id, err)
		return
	}
	start := time.Now()
	d.setStatus(j.id, StatusBuilding)
	d.log.Info("deployment started", "app", j.app, "id", j.id, "kind", j.kind, "commit", j.sha)

	if j.kind == KindRollback {
		lg.Writef("rollback of %s to the build of commit %s", j.app, j.sha[:7])
		err = d.rollback(d.ctx, cfg, j, lg)
	} else {
		lg.Writef("deploy of %s: commit %s of %s (branch %s)", j.app, j.sha[:7], cfg.Repo, j.branch)
		err = d.deploy(d.ctx, cfg, j, lg)
	}
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		lg.Writef("FAILED after %s: %v", took, err)
		d.log.Error("deployment failed", "app", j.app, "id", j.id, "err", err)
	} else {
		lg.Writef("SUCCEEDED in %s", took)
		d.log.Info("deployment succeeded", "app", j.app, "id", j.id, "took", took)
	}
	lg.Close()
	d.finish(j.id, err)
	d.pruneDeployLogs(j.app)
}

// GitHubToken returns the stored GitHub token ("" if none).
func (d *Deployer) GitHubToken(ctx context.Context) (string, error) {
	v, ok, err := d.Store.GetSetting(ctx, SettingGitHubToken)
	if err != nil || !ok {
		return "", err
	}
	b, err := d.Secrets.Open(v, purposeGitHubToken)
	if err != nil {
		return "", fmt.Errorf("stored GitHub token: %w", err)
	}
	return string(b), nil
}

// SetGitHubToken stores the account-level GitHub token encrypted
// (empty removes it).
func (d *Deployer) SetGitHubToken(ctx context.Context, token string) error {
	if token == "" {
		_, err := d.Store.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingGitHubToken)
		return err
	}
	sealed, err := d.Secrets.Seal([]byte(token), purposeGitHubToken)
	if err != nil {
		return err
	}
	return d.Store.SetSetting(ctx, SettingGitHubToken, sealed)
}

func (d *Deployer) deploy(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	b, fresh, err := d.buildCommit(ctx, cfg, j, lg)
	if err != nil {
		return err
	}
	// 5-9. Switch with health check; go back on failure.
	if err := d.activate(ctx, cfg, b, j.id, lg); err != nil {
		if fresh {
			d.removeBuild(cfg.Base.Name, b.ID, lg)
		}
		return err
	}
	d.pruneBuilds(cfg.Base.Name, lg)
	return nil
}

// Prepare builds commit sha of an app (or reuses a kept build) and makes
// it the app's current build without starting it: a server taking over
// from another builds every app before the switch (docs/architecture.md
// §10). It runs outside the queue; the deployment is recorded as kind
// "move". The app must be stopped.
func (d *Deployer) Prepare(ctx context.Context, appName, sha, branch string) (int64, error) {
	cfg, ok := d.Config(appName)
	if !ok {
		return 0, fmt.Errorf("unknown app %q", appName)
	}
	id, err := d.insertDeployment(ctx, appName, KindMove, sha, branch)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.pending[appName] = id
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.pending, appName)
		d.mu.Unlock()
	}()
	lg, err := logs.Open(d.Layout.DeployLog(appName, id), logs.Options{MaxSize: 64 << 20, Keep: 1, RingSize: 64})
	if err != nil {
		d.finish(id, err)
		return id, err
	}
	defer lg.Close()
	d.setStatus(id, StatusBuilding)
	lg.Writef("move: building commit %s of %s (branch %s) on the new server", shortSHA(sha), cfg.Repo, branch)
	b, _, err := d.buildCommit(ctx, cfg, job{id: id, app: appName, kind: KindMove, sha: sha, branch: branch}, lg)
	if err == nil {
		err = d.swapCurrent(appName, b.ID)
	}
	if err == nil {
		if a := d.Supervisor.Get(appName); a != nil {
			err = a.Update(ctx, d.specFor(cfg, b))
		}
	}
	if err != nil {
		lg.Writef("FAILED: %v", err)
	} else {
		lg.Writef("built; it starts when the move switches over")
	}
	d.finish(id, err)
	return id, err
}

// buildCommit returns the build of j.sha: a kept one, or a new one made
// from the source (fresh=true). The app keeps running meanwhile.
func (d *Deployer) buildCommit(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) (b Build, fresh bool, err error) {
	name, id := cfg.Base.Name, BuildID(j.sha)

	// A build still kept on the server needs no rebuild, unless its files
	// have gone missing: then it is built again.
	if kept, err := d.build(ctx, name, id); err == nil && kept.GitSHA == j.sha {
		if d.buildIntact(kept) {
			lg.Writef("commit %s is already built and kept on this server; switching to it (no rebuild)", kept.Short())
			return kept, false, nil
		}
		lg.Writef("the build of %s is recorded but its files are missing; building it again", kept.Short())
		d.removeBuild(name, id, lg)
	}
	if d.Builder == nil {
		return b, false, errors.New("builds are not available on this server")
	}

	// 1. The commit and its source archive. The app keeps serving during
	// steps 1-5.
	api, err := d.github(ctx)
	if err != nil {
		return b, false, err
	}
	commit, err := api.Commit(ctx, cfg.Repo, j.sha)
	if err != nil {
		return b, false, err
	}
	lg.Writef("commit %s: %s (%s, %s)", commit.Short(), commit.Subject, commit.Author, commit.Date.UTC().Format(time.DateTime+" UTC"))

	ws, err := d.Builder.Prepare(name, j.id)
	if err != nil {
		return b, false, err
	}
	defer d.Builder.Remove(ws)
	if err := api.Tarball(ctx, cfg.Repo, j.sha, ws.Archive, source.MaxArchive); err != nil {
		return b, false, err
	}
	if fi, err := os.Stat(ws.Archive); err == nil {
		lg.Writef("downloaded the source archive (%.1f MB)", float64(fi.Size())/(1<<20))
	}

	// 2. Unpack and read goboat.toml.
	if err := source.Unpack(ws.Archive, ws.Src, source.SourceLimits); err != nil {
		return b, false, fmt.Errorf("unpack the source: %w", err)
	}
	os.Remove(ws.Archive)
	m, err := manifest.Load(ws.Src)
	if err != nil {
		return b, false, err
	}
	lg.Writef("goboat.toml: package %s, args %q, health %s", m.Package, m.Args, m.HealthPath)

	// 3. Build and check the binary.
	res, err := d.Builder.Build(ctx, name, j.id, ws, m.Package, lg)
	if res.Took > 0 {
		d.setBuildStats(j.id, res)
	}
	if err != nil {
		return b, false, err
	}
	if err := source.CheckBinary(ws.Binary, runtime.GOARCH); err != nil {
		return b, false, err
	}
	lg.Writef("built with %s", res.GoVersion)

	// 4. Keep it: root-owned, readable only through the app's builds dir.
	dir := d.Layout.BuildDir(name, id)
	os.RemoveAll(dir) // a stray directory from an interrupted deploy
	if err := os.Mkdir(dir, 0o755); err != nil {
		return b, false, fmt.Errorf("store build: %w", err)
	}
	if err := os.Rename(ws.Src, d.Layout.BuildSrc(name, id)); err != nil {
		os.RemoveAll(dir)
		return b, false, fmt.Errorf("store build: %w", err)
	}
	bin := d.Layout.BuildBinary(name, id)
	err = os.Rename(ws.Binary, bin)
	if err == nil {
		err = os.Chown(bin, 0, 0) // the build user wrote it; the app must not be able to change it
	}
	if err == nil {
		err = os.Chmod(bin, 0o755)
	}
	if err != nil {
		os.RemoveAll(dir)
		return b, false, fmt.Errorf("store build: %w", err)
	}
	var size int64
	if fi, err := os.Stat(bin); err == nil {
		size = fi.Size()
	}
	branch := j.branch
	if branch == "" {
		branch, _ = d.BranchOf(ctx, cfg)
	}
	b = Build{App: name, ID: id, GitSHA: j.sha, Branch: branch, Subject: commit.Subject, GoVersion: res.GoVersion,
		Package: m.Package, Args: m.Args, HealthPath: m.HealthPath, Size: size, BuildTime: res.Took, CreatedAt: time.Now()}
	if err := d.insertBuild(ctx, b); err != nil {
		os.RemoveAll(dir)
		return b, false, fmt.Errorf("record build: %w", err)
	}

	return b, true, nil
}

func (d *Deployer) rollback(ctx context.Context, cfg AppConfig, j job, lg *logs.Log) error {
	b, err := d.build(ctx, cfg.Base.Name, BuildID(j.sha))
	if err != nil {
		return err
	}
	if !d.buildIntact(b) {
		return fmt.Errorf("the files of build %s are missing on this server; deploy commit %s from the commit list instead (it is built again)", b.Short(), b.Short())
	}
	return d.activate(ctx, cfg, b, j.id, lg)
}

// buildIntact reports whether a kept build's binary and source are on disk.
func (d *Deployer) buildIntact(b Build) bool {
	fi, err := os.Stat(d.Layout.BuildBinary(b.App, b.ID))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	st, err := os.Stat(d.Layout.BuildSrc(b.App, b.ID))
	return err == nil && st.IsDir()
}

// activate stops the old build, runs the pre-deploy hook, switches the
// `current` symlink and starts b. If b does not become healthy, the
// previous build is restored.
func (d *Deployer) activate(ctx context.Context, cfg AppConfig, b Build, id int64, lg *logs.Log) error {
	name := cfg.Base.Name
	a := d.Supervisor.Get(name)
	if a == nil {
		return fmt.Errorf("app %s is not registered", name)
	}
	u, err := users.Lookup(name)
	if err != nil {
		return err
	}
	prevSpec := a.Spec()
	prevState := a.Status().State
	wasRunning := prevState == supervisor.Running || prevState == supervisor.Starting || prevState == supervisor.Backoff
	prevName := shortSHA(prevSpec.Commit)
	d.setStatus(id, StatusDeploying)

	d.mu.Lock()
	d.deploying[name] = true
	d.mu.Unlock()
	downStart := time.Now()
	defer func() {
		d.mu.Lock()
		delete(d.deploying, name)
		d.mu.Unlock()
	}()

	// Detached from the job context so a shutdown mid-switch still leaves
	// a consistent state.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	if prevSpec.BuildID != "" {
		lg.Writef("stopping the build of %s (state %s)", prevName, prevState)
	}
	if err := a.Stop(sctx); err != nil {
		return fmt.Errorf("stop the old build: %w", err)
	}

	restorePrev := func(reason error) error {
		if prevSpec.BuildID == "" {
			os.Remove(d.Layout.CurrentLink(name))
			a.Update(sctx, cfg.Base)
			lg.Writef("no previous build to go back to; the app stays stopped")
			return reason
		}
		if err := d.swapCurrent(name, prevSpec.BuildID); err != nil {
			return fmt.Errorf("%v; restoring the previous build also failed: %w", reason, err)
		}
		if err := a.Update(sctx, prevSpec); err != nil {
			return fmt.Errorf("%v; restoring the previous build also failed: %w", reason, err)
		}
		if !wasRunning {
			lg.Writef("switched back to the build of %s (left stopped, as it was before)", prevName)
			return fmt.Errorf("%w; switched back to the build of %s", reason, prevName)
		}
		if err := a.Start(sctx); err != nil {
			lg.Writef("the previous build %s also failed to start: %v", prevName, err)
			return fmt.Errorf("%w; went back to the build of %s, which also failed to start: %v", reason, prevName, err)
		}
		lg.Writef("went back: the build of %s is serving again", prevName)
		return fmt.Errorf("%w; went back to the build of %s", reason, prevName)
	}

	if d.PreDeploy != nil {
		if err := d.PreDeploy(sctx, name, lg); err != nil {
			return restorePrev(fmt.Errorf("pre-deploy backup failed: %w", err))
		}
	}
	if err := d.swapCurrent(name, b.ID); err != nil {
		return restorePrev(fmt.Errorf("switch build: %w", err))
	}
	if err := d.Layout.WipeTmp(name, u); err != nil {
		lg.Writef("warning: %v", err)
	}
	newSpec := d.specFor(cfg, b)
	if err := a.Update(sctx, newSpec); err != nil {
		return restorePrev(err)
	}
	lg.Writef("starting the build of %s", b.Short())
	if err := a.Start(sctx); err != nil {
		lg.Writef("the build of %s is not healthy: %v", b.Short(), err)
		if e := a.Stop(sctx); e != nil {
			lg.Writef("warning: stopping the failed build: %v", e)
		}
		return restorePrev(fmt.Errorf("the new build failed its health check: %w", err))
	}
	lg.Writef("the build of %s is healthy; downtime %s", b.Short(), time.Since(downStart).Round(10*time.Millisecond))
	if err := d.SetDesired(sctx, name, true); err != nil {
		lg.Writef("warning: %v", err)
	}
	return nil
}

func shortSHA(s string) string {
	if s == "" {
		return "(none)"
	}
	return s[:min(7, len(s))]
}

// swapCurrent atomically points apps/<app>/current at builds/<id>.
func (d *Deployer) swapCurrent(appName, buildID string) error {
	link := d.Layout.CurrentLink(appName)
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(filepath.Join("builds", buildID), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (d *Deployer) removeBuild(appName, id string, lg *logs.Log) {
	if d.currentBuild(appName) == id {
		return
	}
	if err := os.RemoveAll(d.Layout.BuildDir(appName, id)); err != nil {
		lg.Writef("warning: removing build %s: %v", id, err)
	}
	if err := d.deleteBuild(appName, id); err != nil {
		lg.Writef("warning: forgetting build %s: %v", id, err)
	}
}

// pruneBuilds keeps the current build plus the newest others, up to
// KeepBuilds in total, and deletes stray build directories.
func (d *Deployer) pruneBuilds(appName string, lg *logs.Log) {
	bs, err := d.Builds(context.Background(), appName)
	if err != nil {
		lg.Writef("warning: listing builds: %v", err)
		return
	}
	keep := map[string]bool{}
	for _, b := range bs {
		if b.Current {
			keep[b.ID] = true
		}
	}
	for _, b := range bs {
		if len(keep) >= KeepBuilds {
			break
		}
		keep[b.ID] = true
	}
	for _, b := range bs {
		if !keep[b.ID] {
			lg.Writef("removing the old build of %s", b.Short())
			d.removeBuild(appName, b.ID, lg)
		}
	}
	entries, _ := os.ReadDir(d.Layout.BuildsDir(appName))
	for _, e := range entries {
		if !keep[e.Name()] {
			os.RemoveAll(filepath.Join(d.Layout.BuildsDir(appName), e.Name()))
		}
	}
}

func (d *Deployer) pruneDeployLogs(appName string) {
	dir := d.Layout.DeployLogsDir(appName)
	entries, _ := os.ReadDir(dir)
	var ids []int64
	for _, e := range entries {
		base := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".1"), ".log")
		if n, err := strconv.ParseInt(base, 10, 64); err == nil && !containsID(ids, n) {
			ids = append(ids, n)
		}
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] > ids[k] })
	for _, id := range ids[min(len(ids), KeepDeployLogs):] {
		os.Remove(d.Layout.DeployLog(appName, id))
		os.Remove(d.Layout.DeployLog(appName, id) + ".1")
	}
}

func containsID(ids []int64, n int64) bool {
	for _, x := range ids {
		if x == n {
			return true
		}
	}
	return false
}

// Apps returns the registered app names (sorted).
func (d *Deployer) Apps() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.apps))
	for n := range d.apps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Config returns an app's deploy configuration.
func (d *Deployer) Config(appName string) (AppConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.apps[appName]
	return c, ok
}

// Pending returns the queued or running deployment of app (0 if none).
func (d *Deployer) Pending(appName string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending[appName]
}
