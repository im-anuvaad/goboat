// Package github is a minimal GitHub REST client: token validation, repos,
// branches, commits and the source archive of a commit, which goboat builds
// (docs/architecture.md §7). The token is only sent to the API host.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// DefaultBase is the GitHub REST API.
const DefaultBase = "https://api.github.com"

// API is a minimal GitHub REST client.
type API struct {
	Token string
	Base  string // default DefaultBase (unit tests use a local server)
	HTTP  *http.Client
}

func (a *API) base() string {
	if a.Base != "" {
		return a.Base
	}
	return DefaultBase
}

// Repo is a github.com repository.
type Repo struct {
	Owner string
	Name  string
}

// URL is the repository's web address.
func (r Repo) URL() string { return "https://github.com/" + r.Owner + "/" + r.Name }

// String is owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var repoRe = regexp.MustCompile(`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})$`)

// ParseRepo accepts "owner/repo", "github.com/owner/repo" and
// "https://github.com/owner/repo(.git)".
func ParseRepo(s string) (Repo, error) {
	orig := strings.TrimSpace(s)
	s = strings.TrimPrefix(orig, "https://")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	m := repoRe.FindStringSubmatch(s)
	if m == nil || m[2] == "." || m[2] == ".." {
		return Repo{}, fmt.Errorf("unsupported repository %q: use https://github.com/<owner>/<repo>", orig)
	}
	return Repo{Owner: m[1], Name: m[2]}, nil
}

// TokenInfo describes what a token can do.
type TokenInfo struct {
	Login  string
	Scopes string // classic tokens only (X-OAuth-Scopes)
}

// RepoInfo is one repository the token can read.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// ErrNotFound means the API answered 404 (or 403 for private data without
// access).
var ErrNotFound = errors.New("not found")

func (a *API) do(ctx context.Context, path string, out any) (http.Header, error) {
	base := a.base()
	c := a.HTTP
	if c == nil {
		c = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("github: token is invalid or expired")
	case http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("github: %s: %w (HTTP %d; for a private repository, set a GitHub token with Contents: Read-only in Settings)", path, ErrNotFound, resp.StatusCode)
	default:
		return nil, fmt.Errorf("github: %s: HTTP %d", path, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return nil, fmt.Errorf("github: %s: %w", path, err)
		}
	}
	return resp.Header, nil
}

// Validate checks the token and returns the account it belongs to.
func (a *API) Validate(ctx context.Context) (TokenInfo, error) {
	var u struct {
		Login string `json:"login"`
	}
	h, err := a.do(ctx, "/user", &u)
	if err != nil {
		return TokenInfo{}, err
	}
	return TokenInfo{Login: u.Login, Scopes: h.Get("X-OAuth-Scopes")}, nil
}

// Repos lists repositories the token can access (first 300).
func (a *API) Repos(ctx context.Context) ([]RepoInfo, error) {
	var all []RepoInfo
	for page := 1; page <= 3; page++ {
		var rs []RepoInfo
		if _, err := a.do(ctx, fmt.Sprintf("/user/repos?per_page=100&sort=pushed&page=%d", page), &rs); err != nil {
			return nil, err
		}
		all = append(all, rs...)
		if len(rs) < 100 {
			break
		}
	}
	return all, nil
}

// Commit is one commit of a repository.
type Commit struct {
	SHA     string
	Subject string // first line of the message
	Author  string
	Date    time.Time
}

// Short is the first 7 characters of the SHA.
func (c Commit) Short() string { return c.SHA[:min(7, len(c.SHA))] }

type apiCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (c apiCommit) commit() Commit {
	subject, _, _ := strings.Cut(c.Commit.Message, "\n")
	d := c.Commit.Committer.Date
	if d.IsZero() {
		d = c.Commit.Author.Date
	}
	return Commit{SHA: c.SHA, Subject: strings.TrimSpace(subject), Author: c.Commit.Author.Name, Date: d}
}

func repoPath(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// escapeRef escapes a branch name for a URL path, keeping its slashes.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidSHA reports whether s is a full lowercase commit SHA.
func ValidSHA(s string) bool { return shaRe.MatchString(s) }

// ValidBranch checks a branch name well enough to put it in a URL and a
// form (git's own rules are stricter; GitHub enforces them).
func ValidBranch(b string) error {
	if b == "" || len(b) > 255 || strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.ContainsAny(b, " ~^:?*[\\\x00\t\r\n") {
		return fmt.Errorf("%q is not a valid branch name", b)
	}
	return nil
}

// Repo returns a repository's details (its default branch).
func (a *API) Repo(ctx context.Context, r Repo) (RepoInfo, error) {
	var info RepoInfo
	if _, err := a.do(ctx, repoPath(r), &info); err != nil {
		if errors.Is(err, ErrNotFound) {
			return info, fmt.Errorf("repository %s not found (for a private repository, set a GitHub token with Contents: Read-only in Settings)", r)
		}
		return info, err
	}
	return info, nil
}

// Branches lists up to 300 branch names of r.
func (a *API) Branches(ctx context.Context, r Repo) ([]string, error) {
	var out []string
	for page := 1; page <= 3; page++ {
		var bs []struct {
			Name string `json:"name"`
		}
		if _, err := a.do(ctx, fmt.Sprintf("%s/branches?per_page=100&page=%d", repoPath(r), page), &bs); err != nil {
			return nil, err
		}
		for _, b := range bs {
			out = append(out, b.Name)
		}
		if len(bs) < 100 {
			break
		}
	}
	return out, nil
}

// BranchExists reports whether r has the branch (nil error = it exists).
func (a *API) BranchExists(ctx context.Context, r Repo, branch string) error {
	if err := ValidBranch(branch); err != nil {
		return err
	}
	if _, err := a.do(ctx, repoPath(r)+"/branches/"+escapeRef(branch), nil); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%s has no branch %q", r, branch)
		}
		return err
	}
	return nil
}

// Commits returns the n newest commits of branch, newest first.
func (a *API) Commits(ctx context.Context, r Repo, branch string, n int) ([]Commit, error) {
	if err := ValidBranch(branch); err != nil {
		return nil, err
	}
	var cs []apiCommit
	q := url.Values{"sha": {branch}, "per_page": {fmt.Sprint(min(max(n, 1), 100))}}
	if _, err := a.do(ctx, repoPath(r)+"/commits?"+q.Encode(), &cs); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%s has no branch %q (or it is private and the GitHub token can't read it)", r, branch)
		}
		return nil, err
	}
	out := make([]Commit, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.commit())
	}
	return out, nil
}

// Commit returns one commit by its full SHA.
func (a *API) Commit(ctx context.Context, r Repo, sha string) (Commit, error) {
	if !ValidSHA(sha) {
		return Commit{}, fmt.Errorf("%q is not a full commit SHA", sha)
	}
	var c apiCommit
	if _, err := a.do(ctx, repoPath(r)+"/commits/"+sha, &c); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Commit{}, fmt.Errorf("%s has no commit %s", r, sha[:7])
		}
		return Commit{}, err
	}
	return c.commit(), nil
}

// Tarball writes the source archive (tar.gz) of commit sha of r to path,
// at most max bytes. The API redirects to codeload.github.com; the token is
// not forwarded there (the redirect URL carries its own short-lived token).
func (a *API) Tarball(ctx context.Context, r Repo, sha, path string, max int64) error {
	if !ValidSHA(sha) {
		return fmt.Errorf("%q is not a full commit SHA", sha)
	}
	return a.download(ctx, a.base()+repoPath(r)+"/tarball/"+sha, path, max)
}

func (a *API) download(ctx context.Context, u, path string, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	c := http.Client{} // no overall timeout: ctx bounds the download
	if a.HTTP != nil {
		c = *a.HTTP
	}
	// Never send the token anywhere but the API host (not even to another
	// port of it); Go's own rule compares host names only.
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if r.URL.Host != via[0].URL.Host {
			r.Header.Del("Authorization")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("github: download: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusForbidden:
		return fmt.Errorf("github: download: HTTP %d (for a private repository, set a GitHub token with Contents: Read-only in Settings)", resp.StatusCode)
	default:
		return fmt.Errorf("github: download: HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("the source archive is larger than %d MB", max>>20)
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("github: download: %w", err)
	}
	return nil
}
