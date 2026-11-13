package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/im-anuvaad/goboat/internal/edge"
	"github.com/im-anuvaad/goboat/internal/move"
)

// moveGuard enforces what a move means for the dashboard
// (docs/architecture.md §10): on a server that was moved every page says
// so, and while a move is under way nothing can be changed except the move
// itself.
func (s *Server) moveGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Move == nil || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if m, ok := s.Move.MovedTo(r.Context()); ok {
			s.render(w, r, http.StatusGone, "notice", "Moved", "", map[string]any{
				"Heading": "This server was moved",
				"Message": fmt.Sprintf("Its apps, settings and data were moved to the server %s on %s. It no longer serves anything, takes no backups and changes nothing at Cloudflare. You can delete it.", m.IP, m.At.Local().Format("2 January 2006 15:04 MST")),
			})
			return
		}
		if r.Method == http.MethodPost && s.Move.Frozen() && !moveAllowed(r.URL.Path) {
			redirect(w, r, "/settings/move", errors.New("a move to another server is under way: settings and apps can't be changed until it ends (or is cancelled)"), "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func moveAllowed(path string) bool {
	return strings.HasPrefix(path, "/settings/move/") || path == "/logout" || path == "/login"
}

// installCommand is what the user runs on the new server: the same goboat
// version as this one, with the move code.
func (s *Server) installCommand(code string) string {
	host := s.Edge.DashboardHost()
	ver := s.Version
	if strings.HasPrefix(ver, "v") {
		return fmt.Sprintf("curl -fsSL https://github.com/im-anuvaad/goboat/releases/download/%s/install.sh | sudo GOBOAT_VERSION=%s bash -s -- move https://%s %s", ver, ver, host, code)
	}
	return fmt.Sprintf("curl -fsSL https://github.com/im-anuvaad/goboat/releases/latest/download/install.sh | sudo bash -s -- move https://%s %s", host, code)
}

func (s *Server) movePage(w http.ResponseWriter, r *http.Request) {
	if s.Move == nil {
		s.errorPage(w, r, http.StatusNotFound, errNotFound)
		return
	}
	st := s.Move.State()
	data := map[string]any{"State": st, "Active": st.Active(), "Dashboard": s.Edge.DashboardHost(),
		"DashboardReady": s.Edge.DashboardReady(), "Direct": edge.IsDirect(r)}
	if st.Phase == move.PhaseWaiting && st.Active() {
		data["Command"] = s.installCommand(st.Code)
	}
	if !st.SwitchedAt.IsZero() && st.Phase == move.PhaseSwitching {
		data["SwitchFor"] = time.Since(st.SwitchedAt).Round(time.Second).String()
	}
	if rows, err := s.appRows(r.Context()); err == nil {
		data["Apps"] = rows
	}
	s.render(w, r, http.StatusOK, "move", "Move to another server", "settings", data)
}

func (s *Server) moveStart(w http.ResponseWriter, r *http.Request) {
	if !s.Edge.DashboardReady() {
		redirect(w, r, "/settings/move", errors.New("the dashboard domain must work first: the new server connects to it"), "")
		return
	}
	if err := s.Auth.CheckPassword(r.Context(), r.PostFormValue("password"), edge.ClientIP(r)); err != nil {
		redirect(w, r, "/settings/move", err, "")
		return
	}
	_, err := s.Move.Start(r.Context())
	redirect(w, r, "/settings/move", err, "Move code created. Run the command below on the new server within an hour.")
}

func (s *Server) moveCancel(w http.ResponseWriter, r *http.Request) {
	err := s.Move.Cancel(r.Context())
	redirect(w, r, "/settings/move", err, "Move cancelled. Everything stays on this server.")
}

func (s *Server) moveSwitch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err := s.Move.Switch(ctx)
	redirect(w, r, "/settings/move", err, "Switching: the apps here are stopping and the new server takes over.")
}
