package move

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/im-anuvaad/goboat/internal/secrets"
	"github.com/im-anuvaad/goboat/internal/store"
)

// Phase of a move on the old server.
type Phase string

// Phases.
const (
	PhaseNone      Phase = ""
	PhaseWaiting   Phase = "waiting"   // code shown, no new server yet
	PhaseConnected Phase = "connected" // the new server copied the settings and is building
	PhaseReady     Phase = "ready"     // everything built and pre-copied: Switch now possible
	PhaseSwitching Phase = "switching" // apps stopped here, the new server takes over
	PhaseDone      Phase = "done"      // moved: this server is retired
	PhaseFailed    Phase = "failed"
	PhaseCancelled Phase = "cancelled"
)

// CodeValid is how long a move code can be used to connect.
const CodeValid = time.Hour

// AppInfo is one app as the old server runs it.
type AppInfo struct {
	Name    string `json:"name"`
	Commit  string `json:"commit"` // "" = never deployed
	Branch  string `json:"branch"`
	Running bool   `json:"running"` // should run (desired state)
}

// Hello is the old server's answer to a new server connecting.
type Hello struct {
	Version   string    `json:"version"`
	Dashboard string    `json:"dashboard"`
	Phase     Phase     `json:"phase"`
	Apps      []AppInfo `json:"apps"`
	// Addrs are the old server's public addresses: after the switch it
	// passes Cloudflare's connections on to the new server, which must
	// accept them from there.
	Addrs []string `json:"addrs"`
}

// ForwardFor is how long a retired server passes connections on.
const ForwardFor = 30 * time.Minute

// Step is one line of progress shown on the old dashboard.
type Step struct {
	At     time.Time `json:"at"`
	App    string    `json:"app,omitempty"`
	Text   string    `json:"text"`
	Failed bool      `json:"failed,omitempty"`
}

// State is a move as the old server keeps it (sealed in settings).
type State struct {
	Code       string    `json:"code"`
	Created    time.Time `json:"created"`
	Expires    time.Time `json:"expires"`
	Phase      Phase     `json:"phase"`
	NewIP      string    `json:"new_ip"`
	LastSeen   time.Time `json:"last_seen"`
	Steps      []Step    `json:"steps"`
	Error      string    `json:"error"`
	Running    []string  `json:"running"`  // apps stopped for the switch
	Stopped    bool      `json:"stopped"`  // the apps are stopped: the new server may take the final copy
	SwitchedAt time.Time `json:"switched"` // Switch now pressed
	DoneAt     time.Time `json:"done"`
}

// Active reports whether a move is under way (configuration frozen).
func (s State) Active() bool {
	switch s.Phase {
	case PhaseConnected, PhaseReady, PhaseSwitching:
		return true
	case PhaseWaiting:
		return time.Now().Before(s.Expires)
	}
	return false
}

// Frozen reports whether settings and apps must not change: from the moment
// the new server copied them.
func (s State) Frozen() bool {
	return s.Phase == PhaseConnected || s.Phase == PhaseReady || s.Phase == PhaseSwitching
}

// Moved is where a retired server's installation went.
type Moved struct {
	IP      string    `json:"ip"`
	At      time.Time `json:"at"`
	Forward []string  `json:"forward"` // the new server's addresses
}

// DoneReport is the new server's last message.
type DoneReport struct {
	Addrs []string `json:"addrs"` // its public addresses
}

// SourceHooks connect the old server's side to the rest of goboat.
type SourceHooks struct {
	Apps    func() []AppInfo
	Addrs   func() []string // this server's public addresses
	Bundle  func(ctx context.Context, w io.Writer) error
	Files   func(ctx context.Context, app string, have []FileInfo, w io.Writer) error
	Stop    func(ctx context.Context) (running []string, err error) // router "moving", backups paused, apps stopped
	Restart func(ctx context.Context, running []string)             // undo Stop after a failure or cancel
	Moved   func(m Moved)                                           // retire this server
	// Credential checks share the sign-in rate limit.
	Allow    func(ip string) error
	Fail     func(ip string)
	ClientIP func(r *http.Request) string
	// Direct reports a request on the setup address (refused).
	Direct func(r *http.Request) bool
}

// Source is the old server's side of a move.
type Source struct {
	Store     *store.Store
	Box       *secrets.Box
	Log       *slog.Logger
	Version   string
	Dashboard func() string
	Hooks     SourceHooks

	mu   sync.Mutex
	st   State
	keys Keys
}

// Load reads the saved move. A move that was under way when goboat
// stopped has failed: the apps start as usual and the configuration is no
// longer frozen.
func (s *Source) Load(ctx context.Context) error {
	v, ok, err := s.Store.GetSetting(ctx, SettingSource)
	if err != nil || !ok {
		return err
	}
	b, err := s.Box.Open(v, purposeSource)
	if err != nil {
		return err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = st
	if st.Code != "" {
		s.keys, _ = DeriveKeys(st.Code)
	}
	if st.Frozen() {
		s.failLocked("goboat on this server restarted during the move; the apps run here as before. Start a new move to try again")
		return s.saveLocked(ctx)
	}
	return nil
}

func (s *Source) saveLocked(ctx context.Context) error {
	b, _ := json.Marshal(s.st)
	sealed, err := s.Box.Seal(b, purposeSource)
	if err != nil {
		return err
	}
	return s.Store.SetSetting(context.WithoutCancel(ctx), SettingSource, sealed)
}

func (s *Source) stepLocked(app, text string, failed bool) {
	s.st.Steps = append(s.st.Steps, Step{At: time.Now(), App: app, Text: text, Failed: failed})
	if len(s.st.Steps) > 200 {
		s.st.Steps = s.st.Steps[len(s.st.Steps)-200:]
	}
	if s.Log != nil {
		s.Log.Info("move", "app", app, "step", text)
	}
}

func (s *Source) failLocked(why string) {
	s.st.Phase = PhaseFailed
	s.st.Error = why
	s.stepLocked("", "failed: "+why, true)
}

// State returns the current move.
func (s *Source) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	st.Steps = append([]Step(nil), s.st.Steps...)
	return st
}

// Frozen reports whether configuration changes must be refused.
func (s *Source) Frozen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Frozen()
}

// MovedTo reports whether this server was retired by a move.
func (s *Source) MovedTo(ctx context.Context) (Moved, bool) {
	v, ok, err := s.Store.GetSetting(ctx, SettingMovedTo)
	if err != nil || !ok {
		return Moved{}, false
	}
	var m Moved
	if json.Unmarshal(v, &m) != nil {
		return Moved{}, false
	}
	return m, true
}

// Start creates a new move code, valid for CodeValid.
func (s *Source) Start(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Active() && s.st.Phase != PhaseWaiting {
		return "", errors.New("a move is already under way")
	}
	code := NewCode()
	keys, err := DeriveKeys(code)
	if err != nil {
		return "", err
	}
	s.keys = keys
	s.st = State{Code: code, Created: time.Now(), Expires: time.Now().Add(CodeValid), Phase: PhaseWaiting}
	s.stepLocked("", "move code created; waiting for the new server", false)
	return code, s.saveLocked(ctx)
}

// Cancel ends the move before it is done. If the apps were stopped for the
// switch they start again here.
func (s *Source) Cancel(ctx context.Context) error {
	s.mu.Lock()
	if s.st.Phase == PhaseDone {
		s.mu.Unlock()
		return errors.New("the move is done; this server is retired")
	}
	if !s.st.Active() {
		s.mu.Unlock()
		return errors.New("no move is under way")
	}
	restart := s.st.Phase == PhaseSwitching && s.st.Stopped
	running := s.st.Running
	s.st.Phase = PhaseCancelled
	s.stepLocked("", "cancelled here; the apps keep running on this server", true)
	err := s.saveLocked(ctx)
	s.mu.Unlock()
	if restart {
		s.Hooks.Restart(context.WithoutCancel(ctx), running)
	}
	return err
}

// Switch starts the hand-over: every app here is stopped (visitors see a
// short "moving" page), then the new server takes the final copy of the
// data, starts the apps and points DNS at itself.
func (s *Source) Switch(ctx context.Context) error {
	s.mu.Lock()
	if s.st.Phase != PhaseReady {
		s.mu.Unlock()
		return errors.New("the new server is not ready yet")
	}
	s.st.Phase = PhaseSwitching
	s.st.SwitchedAt = time.Now()
	s.stepLocked("", "switch: stopping the apps here", false)
	s.saveLocked(ctx)
	s.mu.Unlock()

	go func() {
		ctx := context.Background()
		running, err := s.Hooks.Stop(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.st.Phase != PhaseSwitching {
			return
		}
		s.st.Running = running
		if err != nil {
			s.failLocked("stopping the apps for the switch: " + err.Error())
			s.saveLocked(ctx)
			go s.Hooks.Restart(ctx, running)
			return
		}
		s.st.Stopped = true
		s.stepLocked("", fmt.Sprintf("apps stopped (%d were running); the new server takes over", len(running)), false)
		s.saveLocked(ctx)
	}()
	return nil
}

// ---------------------------------------------------------------- API

// Handler serves /move/v1/* for the new server.
func (s *Source) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /move/v1/hello", s.hello)
	mux.HandleFunc("GET /move/v1/bundle", s.bundle)
	mux.HandleFunc("POST /move/v1/progress", s.progress)
	mux.HandleFunc("POST /move/v1/files/{app}", s.files)
	mux.HandleFunc("GET /move/v1/switch", s.waitSwitch)
	mux.HandleFunc("POST /move/v1/done", s.done)
	mux.HandleFunc("POST /move/v1/failed", s.failed)
	return s.auth(mux)
}

func (s *Source) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.Hooks.Direct != nil && s.Hooks.Direct(r) {
			http.Error(w, "moves only work through the dashboard domain", http.StatusForbidden)
			return
		}
		ip := s.Hooks.ClientIP(r)
		if err := s.Hooks.Allow(ip); err != nil {
			w.Header().Set("Retry-After", "900")
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		active := s.st.Active() || (s.st.Phase == PhaseDone && r.URL.Path == "/move/v1/hello")
		ok := active && s.keys.TokenOK(strings.TrimSpace(tok))
		if ok {
			s.st.LastSeen = time.Now()
		}
		s.mu.Unlock()
		if !ok {
			s.Hooks.Fail(ip)
			http.Error(w, "no move with this code is under way here (it may have expired, been cancelled, or the code is wrong)", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		next.ServeHTTP(w, r)
	})
}

func (s *Source) reply(w http.ResponseWriter, path string, v any) {
	b, err := s.keys.Seal("resp:"+path, v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func (s *Source) read(r *http.Request, v any) error {
	return s.keys.Open(r.Body, "req:"+r.URL.Path, 32<<20, v)
}

func (s *Source) hello(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.st.Phase == PhaseWaiting {
		s.st.Phase = PhaseConnected
		s.st.NewIP = s.Hooks.ClientIP(r)
		s.stepLocked("", "the new server ("+s.st.NewIP+") connected; settings and apps can't be changed until the move ends", false)
		s.saveLocked(r.Context())
	}
	h := Hello{Version: s.Version, Dashboard: s.Dashboard(), Phase: s.st.Phase}
	s.mu.Unlock()
	h.Apps = s.Hooks.Apps()
	if s.Hooks.Addrs != nil {
		h.Addrs = s.Hooks.Addrs()
	}
	s.reply(w, r.URL.Path, h)
}

func (s *Source) bundle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ok := s.st.Phase == PhaseConnected
	s.mu.Unlock()
	if !ok {
		http.Error(w, "the settings can only be copied right after connecting", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	enc := s.keys.Encrypt(w, "bundle")
	if err := s.Hooks.Bundle(r.Context(), enc); err != nil {
		s.Log.Error("move: sending the settings", "err", err)
		return // no final frame: the new server sees the stream cut off
	}
	enc.Close()
	s.mu.Lock()
	s.stepLocked("", "settings, certificates and keys copied to the new server", false)
	s.saveLocked(r.Context())
	s.mu.Unlock()
}

// ProgressReport is what the new server reports.
type ProgressReport struct {
	App   string `json:"app"`
	Text  string `json:"text"`
	Ready bool   `json:"ready"`
}

func (s *Source) progress(w http.ResponseWriter, r *http.Request) {
	var p ProgressReport
	if err := s.read(r, &p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.stepLocked(p.App, p.Text, false)
	if p.Ready && s.st.Phase == PhaseConnected {
		s.st.Phase = PhaseReady
		s.stepLocked("", "the new server is ready: press Switch now", false)
	}
	s.saveLocked(r.Context())
	s.mu.Unlock()
	s.reply(w, r.URL.Path, map[string]bool{"ok": true})
}

// FilesRequest asks for an app's DATA_DIR.
type FilesRequest struct {
	Final bool       `json:"final"`
	Have  []FileInfo `json:"have"`
}

func (s *Source) files(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req FilesRequest
	if err := s.read(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	ok := (!req.Final && (s.st.Phase == PhaseConnected || s.st.Phase == PhaseReady)) ||
		(req.Final && s.st.Phase == PhaseSwitching && s.st.Stopped)
	s.mu.Unlock()
	if !ok {
		http.Error(w, "the data can't be copied in this phase of the move", http.StatusConflict)
		return
	}
	kind := "pre"
	if req.Final {
		kind = "final"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	enc := s.keys.Encrypt(w, "files:"+app+":"+kind)
	if err := s.Hooks.Files(r.Context(), app, req.Have, enc); err != nil {
		s.Log.Error("move: sending data", "app", app, "err", err)
		return
	}
	enc.Close()
}

// SwitchAnswer tells a waiting new server what to do.
type SwitchAnswer struct {
	State string `json:"state"` // wait, switch, cancelled, failed
	Error string `json:"error,omitempty"`
}

func (s *Source) waitSwitch(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(45 * time.Second) // under Cloudflare's 100 s
	for {
		s.mu.Lock()
		st := s.st
		s.mu.Unlock()
		a := SwitchAnswer{State: "wait"}
		switch {
		case st.Phase == PhaseSwitching && st.Stopped:
			a.State = "switch"
		case st.Phase == PhaseCancelled:
			a.State = "cancelled"
		case st.Phase == PhaseFailed:
			a = SwitchAnswer{State: "failed", Error: st.Error}
		}
		if a.State != "wait" || time.Now().After(deadline) || r.Context().Err() != nil {
			s.reply(w, r.URL.Path, a)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *Source) done(w http.ResponseWriter, r *http.Request) {
	var rep DoneReport
	s.read(r, &rep)
	s.mu.Lock()
	if s.st.Phase != PhaseSwitching || !s.st.Stopped {
		s.mu.Unlock()
		http.Error(w, "the move is not switching (it may have been cancelled here)", http.StatusConflict)
		return
	}
	s.st.Phase = PhaseDone
	s.st.DoneAt = time.Now()
	s.stepLocked("", fmt.Sprintf("done: the new server serves everything; downtime here %s", time.Since(s.st.SwitchedAt).Round(time.Second)), false)
	s.saveLocked(r.Context())
	m := Moved{IP: s.st.NewIP, At: s.st.DoneAt, Forward: rep.Addrs}
	s.mu.Unlock()
	b, _ := json.Marshal(m)
	if err := s.Store.SetSetting(context.WithoutCancel(r.Context()), SettingMovedTo, b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Hooks.Moved(m)
	s.reply(w, r.URL.Path, map[string]bool{"ok": true})
}

func (s *Source) failed(w http.ResponseWriter, r *http.Request) {
	var p ProgressReport
	s.read(r, &p)
	s.mu.Lock()
	restart := s.st.Phase == PhaseSwitching && s.st.Stopped
	running := s.st.Running
	if s.st.Active() {
		s.failLocked("the new server reported: " + p.Text)
	}
	s.saveLocked(r.Context())
	s.mu.Unlock()
	if restart {
		go s.Hooks.Restart(context.Background(), running)
	}
	s.reply(w, r.URL.Path, map[string]bool{"ok": true})
}
