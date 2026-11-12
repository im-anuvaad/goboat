package web

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/im-anuvaad/goboat/internal/apps"
	"github.com/im-anuvaad/goboat/internal/backup"
	"github.com/im-anuvaad/goboat/internal/deployer"
	"github.com/im-anuvaad/goboat/internal/edge"
	"github.com/im-anuvaad/goboat/internal/github"
	"github.com/im-anuvaad/goboat/internal/hostinfo"
	"github.com/im-anuvaad/goboat/internal/move"
)

// Templates are only checked when executed, so render the main pages with
// representative data: a field that no longer exists fails here, not in
// front of the user.
func TestTemplatesRender(t *testing.T) {
	s := &Server{}
	if err := s.parseTemplates(); err != nil {
		t.Fatal(err)
	}
	row := AppRow{App: apps.App{Name: "web", Repo: "https://github.com/me/web", Branch: "main", Domain: "web.example.com", Port: 20001}}
	row.Status.Commit, row.Status.Branch = "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293", "main"
	commits := []github.Commit{{SHA: "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293", Subject: "Fix", Author: "Me", Date: time.Now()},
		{SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Subject: "Older"}}
	mem, cpu := limitChoices(apps.Input{Memory: "256M", CPU: "1"})
	pages := map[string]any{
		"home": map[string]any{"Apps": []AppRow{row}, "Host": hostinfo.Read(t.TempDir()), "Warnings": []string{"w"}},
		"app": map[string]any{"App": &row, "Range": "1h", "Ranges": []string{"1h"},
			"Builds": []deployer.Build{{App: "web", ID: "3f9c2a1e5b7d", GitSHA: "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293", Branch: "main",
				Subject: "Fix", GoVersion: "go1.27.4", BuildTime: 12 * time.Second, Current: true}, {App: "web", ID: "aaaaaaaaaaaa", GitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			"Branch": "main", "Commits": commits,
			"Edit": apps.Input{Repo: row.Repo}, "EdgeHost": edge.HostStatus{Host: "web.example.com"},
			"MemChoices": mem, "CPUChoices": cpu, "Budget": Budget{Others: 256 << 20, Total: 512 << 20, RAM: 2 << 30},
			"Backups": []backup.Backup{{ID: 1, Status: "ok", LocalPath: "/x"}}, "MoreBackups": true, "Check": backup.Check{At: time.Now(), Unchanged: true, Since: time.Now()}},
		"app_new": map[string]any{"In": apps.Input{RestoreFrom: apps.AutoFolder}, "BucketSet": true, "Folders": []string{"web"},
			"MemChoices": mem, "CPUChoices": cpu, "Budget": Budget{Total: 256 << 20, RAM: 2 << 30}},
		"deployment": map[string]any{"D": deployer.Deployment{ID: 3, App: "web", Status: "succeeded", GitSHA: "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293",
			BuildTime: 63 * time.Second, BuildCPU: 110 * time.Second, PeakMemory: 1 << 30, PeakSwap: 100 << 20}, "Log": "x"},
		"backups": map[string]any{"Name": "web", "Count": 2, "Policy": "every 10m", "Days": []BackupDay{{Day: "Thursday 12 November 2026",
			Backups: []backup.Backup{{ID: 2, App: "web", Kind: "scheduled", Status: "ok", ObjectKey: "web/x", Size: 500, CreatedAt: time.Now()}, {ID: 1, Status: "failed", Error: "boom"}}}}},
		"move": map[string]any{"State": move.State{Phase: move.PhaseReady, NewIP: "198.51.100.2", Steps: []move.Step{{At: time.Now(), App: "web", Text: "built"}, {Text: "x", Failed: true}}},
			"Active": true, "DashboardReady": true, "Apps": []AppRow{row}},
		"settings": map[string]any{"Edge": edge.Status{Dashboard: "d.example.com", TokenSet: true}, "Policy": "every 3h"},
	}
	for name, data := range pages {
		var b bytes.Buffer
		if err := s.pages[name].ExecuteTemplate(&b, "layout", Page{Title: name, CSRF: "x", Data: data}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, st := range []move.State{{}, {Phase: move.PhaseWaiting, Expires: time.Now().Add(time.Hour)}, {Phase: move.PhaseSwitching}, {Phase: move.PhaseDone}, {Phase: move.PhaseFailed, Error: "boom"}} {
		var b bytes.Buffer
		if err := s.pages["move"].ExecuteTemplate(&b, "layout", Page{CSRF: "x", Data: map[string]any{"State": st, "Active": st.Active(), "DashboardReady": true, "Command": "curl"}}); err != nil {
			t.Errorf("move page in phase %q: %v", st.Phase, err)
		}
	}
	// An empty branch: the page says so.
	var b bytes.Buffer
	if err := s.pages["app"].ExecuteTemplate(&b, "layout", Page{Data: map[string]any{"App": &row}}); err != nil || !bytes.Contains(b.Bytes(), []byte("no commits yet")) {
		t.Errorf("app without commits: %v", err)
	}
}

func TestLimitChoices(t *testing.T) {
	mem, cpu := limitChoices(apps.Input{Memory: "512M", CPU: "2"})
	var labels []string
	for _, c := range mem {
		labels = append(labels, c.Label)
		if c.Selected != (c.Value == "512M") {
			t.Errorf("memory %s selected=%v", c.Value, c.Selected)
		}
	}
	if got := strings.Join(labels, ","); got != "128 MB,256 MB,512 MB,1 GB" {
		t.Errorf("memory labels %q", got)
	}
	if len(cpu) != 3 || !cpu[2].Selected || cpu[1].Label != "1 core" {
		t.Errorf("cpu %+v", cpu)
	}
	// A value from before the fixed choices stays selectable.
	mem, _ = limitChoices(apps.Input{Memory: "384M", CPU: "1"})
	if last := mem[len(mem)-1]; last.Value != "384M" || !last.Selected || !strings.Contains(last.Label, "current") {
		t.Errorf("legacy value: %+v", last)
	}
}

func TestBudgetWarning(t *testing.T) {
	if w := (Budget{Total: 1 << 30, RAM: 2 << 30}).Warning(); w != "" {
		t.Errorf("50%%: %q", w)
	}
	if w := (Budget{Total: 1536<<20 + 1, RAM: 2 << 30}).Warning(); !strings.Contains(w, "75%") && !strings.Contains(w, "add up to") {
		t.Errorf("over 75%%: %q", w)
	}
}
