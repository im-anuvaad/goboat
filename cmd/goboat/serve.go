package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"github.com/im-anuvaad/goboat/internal/apps"
	"github.com/im-anuvaad/goboat/internal/auth"
	"github.com/im-anuvaad/goboat/internal/backup"
	"github.com/im-anuvaad/goboat/internal/builder"
	"github.com/im-anuvaad/goboat/internal/buildinfo"
	"github.com/im-anuvaad/goboat/internal/cgroup"
	"github.com/im-anuvaad/goboat/internal/deployer"
	"github.com/im-anuvaad/goboat/internal/edge"
	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/logs"
	"github.com/im-anuvaad/goboat/internal/metrics"
	"github.com/im-anuvaad/goboat/internal/move"
	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
	"github.com/im-anuvaad/goboat/internal/supervisor"
	"github.com/im-anuvaad/goboat/internal/users"
	"github.com/im-anuvaad/goboat/internal/web"
)

func runServe(stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := serve(log); err != nil {
		log.Error("goboat stopped with an error", "err", err)
		return 1
	}
	return 0
}

func serve(log *slog.Logger) error {
	if os.Geteuid() != 0 {
		return errors.New("goboat serve must run as root (it manages users and cgroups)")
	}
	log.Info("starting", "version", buildinfo.String())
	tuneMemory()

	lay := layout.Default()
	if err := layout.EnsureBase(lay); err != nil {
		return err
	}
	box, err := secrets.LoadOrCreate(layout.MasterKey)
	if err != nil {
		return err
	}

	ctx := context.Background()
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	if err := os.Chmod(lay.DBPath(), 0o600); err != nil {
		return err
	}
	ver, _ := st.SchemaVersion(ctx)
	log.Info("state database ready", "path", lay.DBPath(), "schema", ver)

	cg, err := cgroup.Setup("")
	if err != nil {
		return err
	}
	log.Info("cgroup tree ready", "root", cg.Root(), "controllers", keys(cg.Controllers()))
	if cleaned, err := cg.ReapStale(ctx); err != nil {
		return err
	} else if len(cleaned) > 0 {
		log.Warn("killed leftover processes from a previous run", "groups", cleaned)
	}

	sup := supervisor.New(supervisor.Deps{Layout: lay, Cgroups: cg, Log: log})
	bld := &builder.Builder{Layout: lay, Cgroups: cg, Log: log.With("component", "builder")}
	if err := bld.Init(); err != nil {
		return err
	}
	dep, err := deployer.New(ctx, deployer.Deps{
		Layout: lay, Store: st, Secrets: box, Supervisor: sup, Builder: bld, Log: log,
	})
	if err != nil {
		return err
	}

	runCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	authSvc := auth.New(st)
	ms := &moveState{}
	edgeMgr, err := newEdge(ctx, log, lay, st, box, sup, dep, authSvc, ms)
	if err != nil {
		return err
	}
	incoming, isIncoming, err := move.LoadIncoming(ctx, st, box)
	if err != nil {
		return fmt.Errorf("reading the incoming move: %w", err)
	}
	if isIncoming {
		edgeMgr.Pause("this server is taking over from " + incoming.URL)
		log.Info("taking over from another server: Cloudflare syncs, backups and app starts wait for the switch", "from", incoming.URL)
	}

	bk := newBackups(lay, st, box, sup, dep, log)
	bk.Paused = func() bool { return ms.handingOver.Load() || ms.moved.Load() }
	iv, rt := bk.Policy()
	log.Info("backups", "every", iv, "keep", rt)
	if err := bk.Init(ctx); err != nil {
		return err
	}
	dep.PreDeploy = func(ctx context.Context, name string, lg *logs.Log) error {
		_, err := bk.Run(ctx, name, backup.KindPreDeploy, false, lg)
		if errors.Is(err, backup.ErrNoDatabases) {
			lg.Writef("pre-deploy backup: no SQLite databases in DATA_DIR yet; nothing to back up")
			return nil
		}
		return err
	}

	appSvc := apps.New(st, box, dep, sup, edgeMgr, edgeMgr.DashboardHost, log)
	appSvc.SetBackups(bk)
	mv := newMoveSource(lay, st, box, log, authSvc, sup, dep, edgeMgr, bk, ms)
	if err := mv.Load(ctx); err != nil {
		log.Warn("reading the move state", "err", err)
	}
	if m, ok := mv.MovedTo(ctx); ok {
		ms.moved.Store(true)
		edgeMgr.Pause(fmt.Sprintf("this server was moved to %s on %s", m.IP, m.At.UTC().Format(time.DateOnly)))
		log.Warn("this server was moved to another one: it serves no apps, takes no backups and changes nothing at Cloudflare", "new_server", m.IP, "at", m.At)
	}
	// Cleanup never touches a configured domain, even of an app that fails
	// to load.
	edgeMgr.SetKnownHosts(func(ctx context.Context) ([]string, error) {
		list, err := appSvc.List(ctx)
		if err != nil {
			return nil, err
		}
		hosts := []string{edgeMgr.DashboardHost()}
		for _, a := range list {
			hosts = append(hosts, a.Domain)
		}
		return hosts, nil
	})
	if err := appSvc.Load(ctx); err != nil {
		return err
	}
	for _, rt := range appSvc.Routes() {
		if dh := edgeMgr.DashboardHost(); dh != "" && rt.Host == dh {
			log.Error("an app uses the dashboard domain; it is not routed", "app", rt.App, "domain", rt.Host)
		}
	}

	mc := &metrics.Collector{Store: st, Sup: sup, DataRoot: lay.Root, Log: log.With("component", "metrics"),
		Requests: edgeMgr.Router.Stats, Building: func() bool { return bld.Busy(time.Minute) }}

	dash := &web.Server{
		Auth: authSvc, Apps: appSvc, Dep: dep, Sup: sup, Edge: edgeMgr, Store: st,
		Backups: bk, Metrics: mc, Move: mv,
		Thresholds: web.Thresholds{DiskPercent: 85, MemoryPercent: 90, CertDays: 14},
		Layout:     lay, Version: buildinfo.Version, Log: log.With("component", "web"),
	}
	h, err := dash.Handler()
	if err != nil {
		return err
	}
	edgeMgr.Router.Dashboard = h
	if edgeMgr.DashboardHost() == "" {
		log.Warn("no dashboard domain yet: the dashboard is only on the setup address (https://<server IP>)")
	}
	appSvc.RefreshEdge()
	edgeErr := make(chan error, 1)
	go func() { edgeErr <- edgeMgr.Serve(runCtx) }()
	edgeMgr.Run(runCtx)

	dep.Run(runCtx)
	mc.Run(runCtx)
	switch {
	case ms.moved.Load():
		// Retired: nothing runs.
	case isIncoming:
		go runIncoming(runCtx, incoming, lay, st, log, sup, dep, edgeMgr, bk, appSvc, stop)
	default:
		bk.Schedule(runCtx)
		go sup.StartAll(runCtx, func(name string) bool { return dep.DesiredRunning(runCtx, name) })
	}

	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	defer signal.Stop(usr1)

loop:
	for {
		select {
		case <-usr1:
			logStatus(log, sup)
		case err := <-edgeErr:
			if err != nil {
				stop()
				dep.Stop()
				sup.StopAll(context.Background())
				return err
			}
		case <-runCtx.Done():
			break loop
		}
	}

	log.Info("shutting down: cancelling deployments")
	dep.Stop()
	waitUploads(log, bk, 20*time.Second)
	mc.Wait()
	log.Info("stopping apps")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.StopAll(stopCtx); err != nil {
		return fmt.Errorf("stopping apps: %w", err)
	}
	log.Info("all apps stopped; bye")
	return nil
}

// newBackups creates the backup service and connects it to the apps.
func newBackups(lay layout.Layout, st *store.Store, box *secrets.Box, sup *supervisor.Supervisor, dep *deployer.Deployer, log *slog.Logger) *backup.Service {
	return &backup.Service{
		Layout: lay, Store: st, Box: box, Log: log.With("component", "backup"),
		Hooks: backup.Hooks{
			Apps: func() []string {
				var names []string
				for _, a := range sup.Apps() {
					names = append(names, a.Spec().Name)
				}
				return names
			},
			Commit: func(name string) string {
				if a := sup.Get(name); a != nil {
					return a.Status().Commit
				}
				return ""
			},
			Reserve: func(name string) (func(), error) { return dep.Reserve(name, "a restore") },
			Stop: func(ctx context.Context, name string) (bool, error) {
				a := sup.Get(name)
				if a == nil {
					return false, fmt.Errorf("unknown app %q", name)
				}
				switch a.Status().State {
				case supervisor.Stopped, supervisor.Crashed:
					return false, nil
				}
				return true, a.Stop(ctx)
			},
			Start: func(ctx context.Context, name string) error { return dep.StartReserved(ctx, name) },
			Owner: func(name string) (int, int, error) {
				u, err := users.Lookup(name)
				return int(u.UID), int(u.GID), err
			},
		},
	}
}

// waitUploads gives background backup uploads a moment to finish.
func waitUploads(log *slog.Logger, bk *backup.Service, max time.Duration) {
	done := make(chan struct{})
	go func() { bk.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(max):
		log.Warn("backup uploads still running at shutdown; they will be retried after the restart")
	}
}

// newEdge creates the edge manager and its router (not started yet).
func newEdge(ctx context.Context, log *slog.Logger, lay layout.Layout, st *store.Store, box *secrets.Box,
	sup *supervisor.Supervisor, dep *deployer.Deployer, au *auth.Auth, ms *moveState) (*edge.Manager, error) {
	m, err := edge.NewManager(ctx, edge.Config{
		Listen: ":443", AOP: true, DataRoot: lay.Root,
		SetupPending: func() bool { return !au.SetupUntil().IsZero() },
	}, st, box, log)
	if err != nil {
		return nil, err
	}
	m.Router = &edge.Router{
		Log: log,
		State: func(name string) edge.Availability {
			if ms.handingOver.Load() || ms.moved.Load() {
				return edge.Moving
			}
			if dep.Deploying(name) {
				return edge.Deploying
			}
			a := sup.Get(name)
			if a == nil {
				return edge.NotRunning
			}
			switch a.Status().State {
			case supervisor.Running:
				return edge.Available
			case supervisor.Starting:
				return edge.Starting
			}
			return edge.NotRunning
		},
	}
	m.Router.SetDashboardHost(m.DashboardHost())
	return m, nil
}

// logStatus prints every app's state (send SIGUSR1 to goboat).
func logStatus(log *slog.Logger, sup *supervisor.Supervisor) {
	for _, a := range sup.Apps() {
		s := a.Status()
		attrs := []any{"app", a.Spec().Name, "state", s.State, "pid", s.PID, "commit", s.Commit,
			"restarts", s.Restarts, "oom_kills", s.OOMKills}
		if s.LastExit != "" {
			attrs = append(attrs, "last_exit", s.LastExit)
		}
		if s.LastError != "" {
			attrs = append(attrs, "last_error", s.LastError)
		}
		if st, err := a.Group().Stats(); err == nil {
			attrs = append(attrs, "mem_bytes", st.MemoryCurrent, "pids", st.PidsCurrent, "cpu_usec", st.CPUUsageUsec)
		}
		log.Info("status", attrs...)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tuneMemory keeps goboat inside its 30 MB resident budget: a
// soft heap limit makes the GC run more often before the heap grows, and
// freed memory is handed back to the kernel every 2 minutes instead of
// being kept for reuse. GOMEMLIMIT / GOGC in the environment override.
func tuneMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(12 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	go func() {
		for range time.Tick(2 * time.Minute) {
			debug.FreeOSMemory()
		}
	}()
}
