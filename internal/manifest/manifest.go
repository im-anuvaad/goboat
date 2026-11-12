// Package manifest parses and validates an app's goboat.toml
// (docs/app-contract.md §2).
package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/im-anuvaad/goboat/internal/app"
)

// FileName is the manifest file name at the repository root.
const FileName = "goboat.toml"

const maxSize = 64 << 10

// Manifest is a validated goboat.toml: what to build and how to check the
// app is up. Everything operational (domain, branch, limits, env vars) is
// set in the dashboard.
type Manifest struct {
	Contract   int
	Package    string   // the main package, e.g. "." or "./cmd/web"
	Args       []string // command-line arguments for the binary
	HealthPath string
}

type raw struct {
	Contract   *int      `toml:"contract"`
	Package    *string   `toml:"package"`
	Args       *[]string `toml:"args"`
	HealthPath *string   `toml:"health_path"`
}

// dashboardKeys are settings people expect in a config file that goboat
// takes only from the dashboard; they get a pointed message instead of
// "unknown key".
var dashboardKeys = map[string]string{
	"memory": "limits", "memory_max": "limits", "memory_limit": "limits",
	"cpu": "limits", "cpus": "limits", "cpu_max": "limits", "cpu_limit": "limits",
	"pids": "limits", "pids_max": "limits", "processes": "limits",
	"limits": "limits", "resources": "limits",
	"domain": "domain", "domains": "domain", "branch": "branch",
	"env": "env vars", "environment": "env vars",
}

// Load reads dir/goboat.toml and validates it.
func Load(dir string) (*Manifest, error) {
	p := filepath.Join(dir, FileName)
	st, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s not found at the root of the repository at this commit; add one (docs/app-contract.md §2)", FileName)
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", FileName)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSize {
		return nil, fmt.Errorf("%s is larger than 64 KB", FileName)
	}
	return Parse(b)
}

// Parse validates manifest content, reporting every problem at once.
func Parse(b []byte) (*Manifest, error) {
	var r raw
	md, err := toml.Decode(string(b), &r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	var errs []error
	for _, k := range md.Undecoded() {
		key := k.String()
		top, _, _ := strings.Cut(key, ".")
		if what, ok := dashboardKeys[strings.ToLower(top)]; ok {
			if top == key || len(k) == 1 {
				errs = append(errs, fmt.Errorf("%q: %s are set in the dashboard (the app's Settings), not in %s; remove it", key, what, FileName))
			}
			continue
		}
		errs = append(errs, fmt.Errorf("unknown key %q", key))
	}
	m := &Manifest{Package: ".", HealthPath: app.DefaultHealthPath}

	switch {
	case r.Contract == nil:
		errs = append(errs, fmt.Errorf("contract is required (use contract = %d)", app.ContractVersion))
	case *r.Contract != app.ContractVersion:
		errs = append(errs, fmt.Errorf("contract = %d is not supported by this goboat (supported: %d)", *r.Contract, app.ContractVersion))
	default:
		m.Contract = *r.Contract
	}

	if r.Package != nil {
		if err := checkPackage(*r.Package); err != nil {
			errs = append(errs, err)
		} else {
			m.Package = *r.Package
		}
	}

	if r.Args != nil {
		for _, a := range *r.Args {
			if strings.ContainsRune(a, 0) {
				errs = append(errs, errors.New("args must not contain NUL characters"))
				break
			}
		}
		m.Args = *r.Args
	}

	if r.HealthPath != nil {
		if !strings.HasPrefix(*r.HealthPath, "/") || strings.ContainsAny(*r.HealthPath, " \t\r\n") {
			errs = append(errs, fmt.Errorf("health_path %q must start with '/' and contain no spaces", *r.HealthPath))
		} else {
			m.HealthPath = *r.HealthPath
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid %s:\n%w", FileName, err)
	}
	return m, nil
}

// checkPackage requires one local main package: "." or "./sub/dir",
// staying inside the repository, without patterns or flags.
func checkPackage(p string) error {
	bad := func(why string) error {
		return fmt.Errorf(`package %q %s (examples: ".", "./cmd/web")`, p, why)
	}
	switch {
	case p != "." && !strings.HasPrefix(p, "./"):
		return bad("must start with \".\"")
	case strings.Contains(p, "..."):
		return bad("must name one package, not a pattern")
	case strings.ContainsAny(p, " \t\r\n\\\x00"):
		return bad("must not contain spaces or backslashes")
	}
	if c := path.Clean(p); c == ".." || strings.HasPrefix(c, "../") {
		return bad("must stay inside the repository")
	}
	return nil
}
