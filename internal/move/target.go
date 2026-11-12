package move

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

// Incoming is what the new server keeps (sealed) while it takes over.
type Incoming struct {
	URL  string `json:"url"` // https://<old dashboard domain>
	Code string `json:"code"`
}

// SaveIncoming stores the incoming move in the new server's database.
func SaveIncoming(ctx context.Context, st *store.Store, box *secrets.Box, in Incoming) error {
	b, _ := json.Marshal(in)
	sealed, err := box.Seal(b, purposeIncoming)
	if err != nil {
		return err
	}
	return st.SetSetting(ctx, SettingIncoming, sealed)
}

// LoadIncoming reads the incoming move (ok=false: none).
func LoadIncoming(ctx context.Context, st *store.Store, box *secrets.Box) (Incoming, bool, error) {
	v, ok, err := st.GetSetting(ctx, SettingIncoming)
	if err != nil || !ok {
		return Incoming{}, false, err
	}
	b, err := box.Open(v, purposeIncoming)
	if err != nil {
		return Incoming{}, false, err
	}
	var in Incoming
	return in, true, json.Unmarshal(b, &in)
}

// ClearIncoming forgets the incoming move (it is done).
func ClearIncoming(ctx context.Context, st *store.Store) error {
	_, err := st.Writer().ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, SettingIncoming)
	return err
}

// Client is the new server's side of the protocol.
type Client struct {
	Base string // https://<old dashboard domain>
	keys Keys
	http *http.Client
}

// NewClient checks the URL and code.
func NewClient(base, code string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%q is not the old dashboard's address (https://<dashboard domain>)", base)
	}
	keys, err := DeriveKeys(code)
	if err != nil {
		return nil, err
	}
	return &Client{Base: u.Scheme + "://" + u.Host, keys: keys, http: &http.Client{Timeout: 0}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, req any) (*http.Response, error) {
	var body io.Reader
	if req != nil {
		b, err := c.keys.Seal("req:"+path, req)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.keys.Token)
	r.Header.Set("User-Agent", "goboat-move")
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, fmt.Errorf("reaching the old server: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("the old server answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *Client) call(ctx context.Context, method, path string, req, resp any) error {
	r, err := c.do(ctx, method, path, req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return c.keys.Open(r.Body, "resp:"+path, 16<<20, resp)
}

// Hello connects (the old server freezes its configuration).
func (c *Client) Hello(ctx context.Context) (Hello, error) {
	var h Hello
	return h, c.call(ctx, http.MethodGet, "/move/v1/hello", nil, &h)
}

// Bundle streams the settings bundle to fn.
func (c *Client) Bundle(ctx context.Context, fn func(io.Reader) error) error {
	r, err := c.do(ctx, http.MethodGet, "/move/v1/bundle", nil)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	dec := c.keys.Decrypt(r.Body, "bundle")
	if err := fn(dec); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, dec) // to the authenticated end
	return err
}

// Progress reports a step.
func (c *Client) Progress(ctx context.Context, app, text string, ready bool) error {
	var ok map[string]bool
	return c.call(ctx, http.MethodPost, "/move/v1/progress", ProgressReport{App: app, Text: text, Ready: ready}, &ok)
}

// Files streams app's DATA_DIR (what isn't in have, plus the databases).
func (c *Client) Files(ctx context.Context, app string, final bool, have []FileInfo, fn func(io.Reader) error) error {
	r, err := c.do(ctx, http.MethodPost, "/move/v1/files/"+url.PathEscape(app), FilesRequest{Final: final, Have: have})
	if err != nil {
		return err
	}
	defer r.Body.Close()
	kind := "pre"
	if final {
		kind = "final"
	}
	dec := c.keys.Decrypt(r.Body, "files:"+app+":"+kind)
	if err := fn(dec); err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, dec)
	return err
}

// WaitSwitch waits until the user presses Switch now (or cancels).
func (c *Client) WaitSwitch(ctx context.Context) (SwitchAnswer, error) {
	for {
		var a SwitchAnswer
		if err := c.call(ctx, http.MethodGet, "/move/v1/switch", nil, &a); err != nil {
			return a, err
		}
		if a.State != "wait" {
			return a, nil
		}
		if ctx.Err() != nil {
			return a, ctx.Err()
		}
	}
}

// Done tells the old server to retire.
func (c *Client) Done(ctx context.Context) error {
	var ok map[string]bool
	return c.call(ctx, http.MethodPost, "/move/v1/done", nil, &ok)
}

// Failed tells the old server the move failed (it restarts its apps).
func (c *Client) Failed(ctx context.Context, why string) error {
	var ok map[string]bool
	return c.call(ctx, http.MethodPost, "/move/v1/failed", ProgressReport{Text: why}, &ok)
}

// TargetHooks connect the new server's side to the rest of goboat.
type TargetHooks struct {
	// Prepare builds an app's commit and makes it current (not started).
	Prepare func(ctx context.Context, app, commit, branch string) error
	// DataDir returns an app's DATA_DIR and its owner.
	DataDir func(app string) (dir string, uid, gid int, err error)
	// Start starts an app and waits until it is healthy.
	Start func(ctx context.Context, app string) error
}

// ErrCancelled means the move was cancelled on the old server.
var ErrCancelled = errors.New("the move was cancelled on the old server")

// Run takes over from the old server (docs/architecture.md §10, steps 4-8).
// It returns nil once the old server has retired; the caller then turns
// Cloudflare syncs (DNS points here) and backups on.
func Run(ctx context.Context, c *Client, version string, h TargetHooks, log *slog.Logger) error {
	hello, err := c.Hello(ctx)
	if err != nil {
		return err
	}
	if hello.Phase == PhaseDone {
		return nil // finished before this server restarted
	}
	if hello.Version != version {
		return fmt.Errorf("the old server runs %s and this one %s; update the old server first (run the install command there), then start the move again", hello.Version, version)
	}
	report := func(app, text string) {
		log.Info("move", "app", app, "step", text)
		if err := c.Progress(ctx, app, text, false); err != nil {
			log.Warn("move: reporting progress", "err", err)
		}
	}

	// 4. Build every app at the commit the old server runs, and copy its
	// data while the old server keeps serving.
	if hello.Phase == PhaseConnected || hello.Phase == PhaseReady {
		for _, a := range hello.Apps {
			if a.Commit == "" {
				report(a.Name, "never deployed: nothing to build")
			} else {
				report(a.Name, "building commit "+a.Commit[:min(7, len(a.Commit))])
				start := time.Now()
				if err := h.Prepare(ctx, a.Name, a.Commit, a.Branch); err != nil {
					return fmt.Errorf("building %s: %w", a.Name, err)
				}
				report(a.Name, fmt.Sprintf("built in %s", time.Since(start).Round(time.Second)))
			}
			n, size, err := copyData(ctx, c, h, a.Name, false)
			if err != nil {
				return fmt.Errorf("copying the data of %s: %w", a.Name, err)
			}
			report(a.Name, fmt.Sprintf("data copied (%d files, %s); the final copy at the switch sends only what changed", n, human(size)))
		}
		if err := c.Progress(ctx, "", "every app is built and its data copied", true); err != nil {
			return err
		}
	}

	// 5. Wait for Switch now.
	a, err := c.WaitSwitch(ctx)
	if err != nil {
		return err
	}
	switch a.State {
	case "cancelled":
		return ErrCancelled
	case "failed":
		return errors.New("the move failed on the old server: " + a.Error)
	}

	// 7. Final copy (the apps are stopped there) and start here.
	start := time.Now()
	for _, app := range hello.Apps {
		n, size, err := copyData(ctx, c, h, app.Name, true)
		if err != nil {
			return fmt.Errorf("final copy of the data of %s: %w", app.Name, err)
		}
		report(app.Name, fmt.Sprintf("final data copied (%d files, %s)", n, human(size)))
	}
	for _, app := range hello.Apps {
		if !app.Running || app.Commit == "" {
			continue
		}
		if err := h.Start(ctx, app.Name); err != nil {
			return fmt.Errorf("starting %s here: %w", app.Name, err)
		}
		report(app.Name, "running and healthy on the new server")
	}

	// 8. The old server retires; then the caller points DNS here.
	if err := c.Done(ctx); err != nil {
		return err
	}
	log.Info("move: the old server retired", "switch_took", time.Since(start).Round(100*time.Millisecond))
	return nil
}

func copyData(ctx context.Context, c *Client, h TargetHooks, app string, final bool) (int, int64, error) {
	dir, uid, gid, err := h.DataDir(app)
	if err != nil {
		return 0, 0, err
	}
	have, err := ListDataDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var size int64
	err = c.Files(ctx, app, final, have, func(r io.Reader) error {
		var err error
		n, size, err = UnpackDataDir(r, dir, uid, gid, final)
		return err
	})
	return n, size, err
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
