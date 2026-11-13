package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/im-anuvaad/goboat/internal/apps"
	"github.com/im-anuvaad/goboat/internal/auth"
	"github.com/im-anuvaad/goboat/internal/backup"
	"github.com/im-anuvaad/goboat/internal/buildinfo"
	"github.com/im-anuvaad/goboat/internal/deployer"
	"github.com/im-anuvaad/goboat/internal/edge"
	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/move"
	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
	"github.com/im-anuvaad/goboat/internal/supervisor"
	"github.com/im-anuvaad/goboat/internal/users"
)

// moveState is what serve needs to know about moves (docs/architecture.md
// §10): handing over (apps stopped, router "moving"), moved (retired), or
// incoming (taking over from an old server).
type moveState struct {
	handingOver atomic.Bool
	moved       atomic.Bool
}

// newMoveSource is the old server's side of a move.
func newMoveSource(lay layout.Layout, st *store.Store, box *secrets.Box, log *slog.Logger, au *auth.Auth,
	sup *supervisor.Supervisor, dep *deployer.Deployer, e *edge.Manager, bk *backup.Service, ms *moveState) *move.Source {
	return &move.Source{
		Store: st, Box: box, Log: log.With("component", "move"), Version: buildinfo.Version, Dashboard: e.DashboardHost,
		Hooks: move.SourceHooks{
			Apps: func() []move.AppInfo {
				var out []move.AppInfo
				for _, a := range sup.Apps() {
					sp := a.Spec()
					out = append(out, move.AppInfo{Name: sp.Name, Commit: sp.Commit, Branch: sp.Branch,
						Running: dep.DesiredRunning(context.Background(), sp.Name)})
				}
				return out
			},
			Addrs: func() []string { return e.PublicAddrs(context.Background()) },
			Bundle: func(ctx context.Context, w io.Writer) error {
				return move.MakeBundle(ctx, lay, layout.MasterKey, st.Writer(), w)
			},
			Files: func(ctx context.Context, app string, have []move.FileInfo, w io.Writer) error {
				if sup.Get(app) == nil {
					return fmt.Errorf("unknown app %q", app)
				}
				_, _, err := move.PackDataDir(ctx, lay.DataDir(app), filepath.Join(lay.Root, "backups", "staging"), have, w)
				return err
			},
			Stop: func(ctx context.Context) ([]string, error) {
				// From now on the new server decides what Cloudflare points to.
				e.PauseAndWait("handing over to the new server")
				ms.handingOver.Store(true)
				bk.Quiesce()
				var running []string
				for _, a := range sup.Apps() {
					switch a.Status().State {
					case supervisor.Stopped, supervisor.Crashed:
						continue
					}
					if err := a.Stop(ctx); err != nil {
						return running, err
					}
					running = append(running, a.Spec().Name)
				}
				return running, nil
			},
			Restart: func(ctx context.Context, running []string) {
				ms.handingOver.Store(false)
				e.Resume()
				for _, name := range running {
					if err := dep.Start(ctx, name); err != nil {
						log.Error("move: restarting an app after the move ended", "app", name, "err", err)
					}
				}
			},
			Moved: func(m move.Moved) {
				ms.moved.Store(true)
				e.ForwardTo(m.Forward, m.At.Add(move.ForwardFor))
				e.DropConnections()
				e.PauseAndWait(fmt.Sprintf("this server was moved to %s on %s", m.IP, m.At.UTC().Format(time.DateOnly)))
				log.Info("this server was moved; it no longer serves apps, backs up or changes anything at Cloudflare", "new_server", m.IP)
			},
			Allow:    au.Allow,
			Fail:     au.Fail,
			ClientIP: edge.ClientIP,
			Direct:   edge.IsDirect,
		},
	}
}

// runIncoming takes over from the old server, then turns Cloudflare syncs
// (DNS points here) and backups on. If the move fails, everything received
// is removed and goboat stops: this server is empty again (re-run the
// install command to retry or to set it up fresh).
func runIncoming(ctx context.Context, in move.Incoming, lay layout.Layout, st *store.Store, log *slog.Logger,
	sup *supervisor.Supervisor, dep *deployer.Deployer, e *edge.Manager, bk *backup.Service, appSvc *apps.Service, stop func()) {
	log = log.With("component", "move")
	c, err := move.NewClient(in.URL, in.Code)
	if err == nil {
		err = move.Run(ctx, c, buildinfo.Version, move.TargetHooks{
			Prepare: func(ctx context.Context, app, commit, branch string) error {
				_, err := dep.Prepare(ctx, app, commit, branch)
				return err
			},
			DataDir: func(app string) (string, int, int, error) {
				u, err := users.Lookup(app)
				return lay.DataDir(app), int(u.UID), int(u.GID), err
			},
			Start:  func(ctx context.Context, app string) error { return dep.Start(ctx, app) },
			Accept: func(addrs []string) { e.Filter.AllowTemporarily(addrs, 2*time.Hour+move.ForwardFor) },
			Addrs:  func() []string { return e.PublicAddrs(ctx) },
		}, log)
	}
	if err == nil {
		if err := move.ClearIncoming(context.WithoutCancel(ctx), st); err != nil {
			log.Error("move: forgetting the finished move", "err", err)
		}
		e.Resume()
		e.SyncInBackground() // DNS records now point here
		bk.Schedule(ctx)
		log.Info("move complete: this server serves everything now")
		return
	}
	if ctx.Err() != nil {
		return // shutting down; the move continues after the restart
	}
	confirmed := errors.Is(err, move.ErrCancelled)
	if c != nil && !confirmed {
		fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		ferr := c.Failed(fctx, err.Error())
		cancel()
		confirmed = ferr == nil
		if ferr != nil {
			log.Warn("move: telling the old server", "err", ferr)
		}
	}
	if errors.Is(err, move.ErrAfterSwitch) && !confirmed {
		// The old server has stopped its apps and may have retired: this
		// server may be the only one with them now. Keep everything; goboat
		// restarts and tries again.
		log.Error("the move could not be completed or undone; keeping everything and trying again", "err", err)
		stop()
		return
	}
	log.Error("the move failed; removing everything received from the old server", "err", err)
	sctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sup.StopAll(sctx)
	list, _ := appSvc.List(sctx)
	for _, a := range list {
		os.RemoveAll(lay.AppDir(a.Name))
		users.Remove(a.Name)
	}
	for _, p := range []string{lay.DBPath(), lay.DBPath() + "-wal", lay.DBPath() + "-shm", layout.MasterKey,
		filepath.Join(lay.Root, "aop"), filepath.Join(lay.Root, "certs"), filepath.Join(lay.Root, "backups")} {
		os.RemoveAll(p)
	}
	log.Error("this server is empty again; run the install command again to retry the move or to set goboat up fresh")
	stop()
}
