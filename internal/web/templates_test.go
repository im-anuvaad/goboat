package web

import (
	"bytes"
	"testing"
	"time"

	"github.com/im-anuvaad/goboat/internal/apps"
	"github.com/im-anuvaad/goboat/internal/deployer"
	"github.com/im-anuvaad/goboat/internal/edge"
	"github.com/im-anuvaad/goboat/internal/github"
	"github.com/im-anuvaad/goboat/internal/hostinfo"
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
	pages := map[string]any{
		"home": map[string]any{"Apps": []AppRow{row}, "Host": hostinfo.Read(t.TempDir()), "Warnings": []string{"w"}},
		"app": map[string]any{"App": &row, "Range": "1h", "Ranges": []string{"1h"},
			"Builds": []deployer.Build{{App: "web", ID: "3f9c2a1e5b7d", GitSHA: "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293", Branch: "main",
				Subject: "Fix", GoVersion: "go1.27.4", BuildTime: 12 * time.Second, Current: true}, {App: "web", ID: "aaaaaaaaaaaa", GitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
			"Branch": "main", "Commits": commits,
			"Edit": apps.Input{Repo: row.Repo}, "EdgeHost": edge.HostStatus{Host: "web.example.com"}},
		"app_new":  map[string]any{"In": apps.Input{RestoreFrom: apps.AutoFolder}, "BucketSet": true, "Folders": []string{"web"}},
		"settings": map[string]any{"Edge": edge.Status{Dashboard: "d.example.com", TokenSet: true}, "Policy": "every 3h"},
	}
	for name, data := range pages {
		var b bytes.Buffer
		if err := s.pages[name].ExecuteTemplate(&b, "layout", Page{Title: name, CSRF: "x", Data: data}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An empty branch: the page says so.
	var b bytes.Buffer
	if err := s.pages["app"].ExecuteTemplate(&b, "layout", Page{Data: map[string]any{"App": &row}}); err != nil || !bytes.Contains(b.Bytes(), []byte("no commits yet")) {
		t.Errorf("app without commits: %v", err)
	}
}
