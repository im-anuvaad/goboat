package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/im-anuvaad/goboat/internal/buildinfo"
	"github.com/im-anuvaad/goboat/internal/layout"
	"github.com/im-anuvaad/goboat/internal/move"
	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

func runMoveIn(url, code string, stdout, stderr io.Writer) int {
	if err := moveIn(url, code, stdout); err != nil {
		fmt.Fprintf(stderr, "\ngoboat move-in: %v\n", err)
		return 1
	}
	return 0
}

// moveIn prepares this (empty) server and copies the old server's
// settings, keys and certificates (docs/architecture.md §10, steps 1-3).
// goboat then starts in incoming mode and does the rest.
func moveIn(url, code string, out io.Writer) error {
	c, err := move.NewClient(url, code)
	if err != nil {
		return err
	}
	lay := layout.Default()
	ctx := context.Background()
	if err := mustBeEmpty(ctx, lay); err != nil {
		return err
	}
	if err := prepareHost(out); err != nil {
		return err
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	hello, err := c.Hello(cctx)
	cancel()
	if err != nil {
		return err
	}
	if hello.Version != buildinfo.Version {
		return fmt.Errorf("the old server runs %s and this one %s; use the command shown on the old server's Move page (it installs the same version)", hello.Version, buildinfo.Version)
	}
	fmt.Fprintf(out, "Connected to %s (%d app(s)). Its settings can't be changed until the move ends.\n", hello.Dashboard, len(hello.Apps))

	bctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	err = c.Bundle(bctx, func(r io.Reader) error { return move.InstallBundle(r, lay, layout.MasterKey) })
	if err != nil {
		return fmt.Errorf("copying the settings: %w", err)
	}
	box, err := secrets.Load(layout.MasterKey)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	err = move.SaveIncoming(ctx, st, box, move.Incoming{URL: c.Base, Code: code})
	st.Close()
	if err != nil {
		return err
	}
	os.Chmod(lay.DBPath(), 0o600)
	fmt.Fprintln(out, "Settings, certificates and keys copied.")

	if err := systemctl("start", "goboat"); err != nil {
		return err
	}
	fmt.Fprintf(out, "\ngoboat %s is running and builds every app now; the old server keeps serving meanwhile.\n\n", buildinfo.Version)
	fmt.Fprintf(out, "  Follow the progress and press Switch now on the old dashboard:\n    https://%s/settings/move\n\n", hello.Dashboard)
	fmt.Fprintln(out, "  You can close SSH.")
	return nil
}

// mustBeEmpty refuses a server that already has an account or apps.
func mustBeEmpty(ctx context.Context, lay layout.Layout) error {
	if serviceActive() {
		return errors.New("goboat is running; stop it first (systemctl stop goboat)")
	}
	if _, err := os.Stat(lay.DBPath()); err != nil {
		return nil
	}
	st, err := store.Open(ctx, lay.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	var apps, users int
	st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM apps`).Scan(&apps)
	st.Reader().QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&users)
	if apps > 0 || users > 0 {
		return fmt.Errorf("this server already runs goboat with %d app(s) and an account; a move needs a fresh server (or one where goboat was only installed)", apps)
	}
	return nil
}
