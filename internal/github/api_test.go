package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sha = "3f9c2a1e5b7d4c0a9e8f6b1d2c3a4e5f60718293"

func TestBranchesCommitsAndTarball(t *testing.T) {
	// The codeload host serves the archive; it must never see the token.
	codeload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("token forwarded to the archive host")
		}
		w.Write([]byte("tarball bytes"))
	}))
	defer codeload.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		switch r.URL.EscapedPath() {
		case "/repos/me/app":
			w.Write([]byte(`{"full_name":"me/app","default_branch":"main"}`))
		case "/repos/me/app/branches":
			w.Write([]byte(`[{"name":"main"},{"name":"feature/x"}]`))
		case "/repos/me/app/branches/feature/x", "/repos/me/app/branches/main":
			w.Write([]byte(`{}`))
		case "/repos/me/app/commits":
			if r.URL.Query().Get("sha") != "feature/x" || r.URL.Query().Get("per_page") != "10" {
				t.Errorf("commits query %q", r.URL.RawQuery)
			}
			w.Write([]byte(`[{"sha":"` + sha + `","commit":{"message":"Fix the thing\n\nLonger text","author":{"name":"Me","date":"2026-11-01T10:00:00Z"},"committer":{"date":"2026-11-02T10:00:00Z"}}}]`))
		case "/repos/me/app/commits/" + sha:
			w.Write([]byte(`{"sha":"` + sha + `","commit":{"message":"Fix the thing","author":{"name":"Me"}}}`))
		case "/repos/me/app/tarball/" + sha:
			http.Redirect(w, r, codeload.URL+"/me/app/legacy.tar.gz/"+sha, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	ctx := context.Background()
	a := &API{Token: "tok", Base: api.URL}
	repo := Repo{Owner: "me", Name: "app"}
	if info, err := a.Repo(ctx, repo); err != nil || info.DefaultBranch != "main" {
		t.Fatalf("repo: %+v %v", info, err)
	}
	if bs, err := a.Branches(ctx, repo); err != nil || strings.Join(bs, ",") != "main,feature/x" {
		t.Fatalf("branches: %v %v", bs, err)
	}
	if err := a.BranchExists(ctx, repo, "feature/x"); err != nil {
		t.Fatalf("branch with a slash: %v", err)
	}
	if err := a.BranchExists(ctx, repo, "nope"); err == nil || !strings.Contains(err.Error(), `has no branch "nope"`) {
		t.Fatalf("missing branch: %v", err)
	}
	cs, err := a.Commits(ctx, repo, "feature/x", 10)
	if err != nil || len(cs) != 1 || cs[0].Subject != "Fix the thing" || cs[0].Short() != "3f9c2a1" || cs[0].Date.Day() != 2 {
		t.Fatalf("commits: %+v %v", cs, err)
	}
	if c, err := a.Commit(ctx, repo, sha); err != nil || c.Subject != "Fix the thing" {
		t.Fatalf("commit: %+v %v", c, err)
	}
	if _, err := a.Commit(ctx, repo, "abc"); err == nil {
		t.Fatal("short SHA accepted")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "a.tar.gz")
	if err := a.Tarball(ctx, repo, sha, p, 1<<20); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "tarball bytes" {
		t.Fatalf("got %q", b)
	}
	if err := a.Tarball(ctx, repo, sha, filepath.Join(dir, "small"), 4); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("size limit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "small")); err == nil {
		t.Fatal("a too-large download must not be left behind")
	}

	// Without the token the private repository looks missing.
	anon := &API{Base: api.URL}
	if _, err := anon.Branches(ctx, repo); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("private repo without token: %v", err)
	}
}

func TestValidBranch(t *testing.T) {
	for _, b := range []string{"main", "feature/x", "release-1.2", "v1"} {
		if err := ValidBranch(b); err != nil {
			t.Errorf("%q: %v", b, err)
		}
	}
	for _, b := range []string{"", "/x", "x/", "a..b", "a b", "a:b", "a//b", "x\x00"} {
		if ValidBranch(b) == nil {
			t.Errorf("%q accepted", b)
		}
	}
}
