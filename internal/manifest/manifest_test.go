package manifest

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/im-anuvaad/goboat/internal/app"
)

func TestParse(t *testing.T) {
	m, err := Parse([]byte(`contract = 1
package = "./cmd/web"
args = ["-mode", "prod"]
health_path = "/healthz"
`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Contract != 1 || m.Package != "./cmd/web" || len(m.Args) != 2 || m.HealthPath != "/healthz" {
		t.Fatalf("%+v", m)
	}
	m, err = Parse([]byte("contract = 1\n"))
	if err != nil || m.Package != "." || m.HealthPath != app.DefaultHealthPath || len(m.Args) != 0 {
		t.Fatalf("defaults: %+v %v", m, err)
	}
}

func TestParseReportsEverything(t *testing.T) {
	_, err := Parse([]byte(`contract = 4
package = "cmd/web"
health_path = "healthz"
colour = "blue"
`))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"contract = 4", `must start with "."`, "health_path", `unknown key "colour"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	for _, body := range []string{"", "not toml ===", "contract = 1\npackage = \"../escape\"\n",
		"contract = 1\npackage = \"./...\"\n", "contract = 1\npackage = \"./a/../../b\"\n", "contract = 1\nargs = \"-x\"\n"} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

// Limits, domain, branch and env vars come only from the dashboard: the
// error says so instead of "unknown key".
func TestParseDashboardKeys(t *testing.T) {
	for _, body := range []string{"memory = \"512M\"", "cpu = 2", "[limits]\nmemory = \"1G\"", "domain = \"x.example.com\"", "branch = \"main\""} {
		_, err := Parse([]byte("contract = 1\n" + body + "\n"))
		if err == nil || !strings.Contains(err.Error(), "set in the dashboard") || strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// The release-era contract (run = ...) is refused with the version error.
func TestParseOldContract(t *testing.T) {
	_, err := Parse([]byte("contract = 3\nrun = \"build/app\"\n"))
	if err == nil || !strings.Contains(err.Error(), "contract = 3 is not supported") {
		t.Errorf("contract 3: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "not found at the root of the repository") {
		t.Fatalf("missing: %v", err)
	}
	os.Symlink("/etc/passwd", filepath.Join(dir, FileName))
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink: %v", err)
	}
}

// FuzzParse: never panics, and anything accepted satisfies the contract.
func FuzzParse(f *testing.F) {
	f.Add([]byte("contract = 1\n"))
	f.Add([]byte("contract = 1\npackage = \"./cmd/x\"\nargs = [\"a\", \"b c\"]\nhealth_path = \"/h\"\n"))
	f.Add([]byte("contract = 1\npackage = \"./x/../../y\"\n"))
	f.Add([]byte("[package]\nx = 1\n"))
	f.Add([]byte("contract = 99999999999999999999\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := Parse(b)
		if err != nil {
			return
		}
		if m.Contract != app.ContractVersion {
			t.Fatalf("accepted invalid manifest: %+v", m)
		}
		if m.Package != "." && !strings.HasPrefix(m.Package, "./") {
			t.Fatalf("bad package: %q", m.Package)
		}
		if c := path.Clean(m.Package); c == ".." || strings.HasPrefix(c, "../") || strings.Contains(m.Package, "...") {
			t.Fatalf("package escapes the repository: %q", m.Package)
		}
		if !strings.HasPrefix(m.HealthPath, "/") || strings.ContainsAny(m.HealthPath, " \t\r\n") {
			t.Fatalf("bad health path %q", m.HealthPath)
		}
	})
}
