package supervisor

import (
	"slices"
	"testing"

	"github.com/im-anuvaad/goboat/internal/app"
	"github.com/im-anuvaad/goboat/internal/layout"
)

func TestEnvironGoMemLimit(t *testing.T) {
	a := &App{dirs: layout.Layout{Root: "/r"}}
	spec := app.Spec{Name: "web", Port: 20001, Limits: app.Limits{MemoryMax: 256 << 20}}

	env := a.environ(spec)
	if want := "GOMEMLIMIT=241591905"; !slices.Contains(env, want) { // 256 MiB / 10 * 9
		t.Errorf("missing %s in %v", want, env)
	}
	for _, want := range []string{"GOBOAT_CONTRACT=3", "DATA_DIR=/r/apps/web/data", "PORT=20001"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s in %v", want, env)
		}
	}

	// The app's own GOMEMLIMIT (an env var set in the dashboard) is kept.
	spec.Env = map[string]string{"GOMEMLIMIT": "100MiB"}
	env = a.environ(spec)
	n := 0
	for _, kv := range env {
		if len(kv) > 11 && kv[:11] == "GOMEMLIMIT=" {
			n++
			if kv != "GOMEMLIMIT=100MiB" {
				t.Errorf("user GOMEMLIMIT overridden: %s", kv)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d GOMEMLIMIT entries", n)
	}
}
