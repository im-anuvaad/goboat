package deployer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/im-anuvaad/goboat/internal/builder"
)

// Deployment statuses.
const (
	StatusQueued    = "queued"
	StatusBuilding  = "building" // downloading the source and building it
	StatusDeploying = "deploying"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// Deployment kinds.
const (
	KindDeploy   = "deploy"
	KindRollback = "rollback"
	KindMove     = "move" // built by a server taking over (no switch)
)

// Deployment is one row of the deploy history.
type Deployment struct {
	ID         int64     `json:"id"`
	App        string    `json:"app"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	BuildID    string    `json:"build_id"`
	GitSHA     string    `json:"git_sha"`
	Branch     string    `json:"branch"`
	Error      string    `json:"error"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// What its build used (zero when nothing was built).
	BuildTime  time.Duration `json:"build_time"`
	BuildCPU   time.Duration `json:"build_cpu"`
	PeakMemory int64         `json:"peak_memory"`
	PeakSwap   int64         `json:"peak_swap"`
}

// Built reports whether the deployment ran a build.
func (d Deployment) Built() bool { return d.BuildTime > 0 }

// Done reports whether the deployment has finished.
func (d Deployment) Done() bool { return d.Status == StatusSucceeded || d.Status == StatusFailed }

// Build is a build kept on the server (at most KeepBuilds per app).
type Build struct {
	App        string        `json:"app"`
	ID         string        `json:"id"` // BuildID(GitSHA)
	GitSHA     string        `json:"git_sha"`
	Branch     string        `json:"branch"`
	Subject    string        `json:"subject"`
	GoVersion  string        `json:"go_version"`
	Package    string        `json:"package"`
	Args       []string      `json:"args"`
	HealthPath string        `json:"health_path"`
	Size       int64         `json:"size"`
	BuildTime  time.Duration `json:"build_time"`
	CreatedAt  time.Time     `json:"created_at"`
	Current    bool          `json:"current"`
}

// Short is the first 7 characters of the commit SHA.
func (b Build) Short() string { return b.GitSHA[:min(7, len(b.GitSHA))] }

// BuildID is the id (and directory name) of the build of a commit.
func BuildID(sha string) string { return sha[:min(12, len(sha))] }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

const deploymentCols = `id, app, kind, status, build_id, git_sha, branch, error, created_at, started_at, finished_at,
	build_ms, build_cpu_ms, peak_memory, peak_swap`

func scanDeployment(s interface{ Scan(...any) error }) (Deployment, error) {
	var d Deployment
	var c, st, f, bms, cms int64
	err := s.Scan(&d.ID, &d.App, &d.Kind, &d.Status, &d.BuildID, &d.GitSHA, &d.Branch, &d.Error, &c, &st, &f,
		&bms, &cms, &d.PeakMemory, &d.PeakSwap)
	d.CreatedAt, d.StartedAt, d.FinishedAt = fromUnix(c), fromUnix(st), fromUnix(f)
	d.BuildTime, d.BuildCPU = time.Duration(bms)*time.Millisecond, time.Duration(cms)*time.Millisecond
	return d, err
}

func (d *Deployer) insertDeployment(ctx context.Context, app, kind, sha, branch string) (int64, error) {
	buildID := ""
	if sha != "" {
		buildID = BuildID(sha)
	}
	res, err := d.db.Writer().ExecContext(ctx,
		`INSERT INTO deployments (app, kind, status, build_id, git_sha, branch, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		app, kind, StatusQueued, buildID, sha, branch, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("deployer: record deployment: %w", err)
	}
	return res.LastInsertId()
}

func (d *Deployer) setStatus(id int64, status string) {
	col := ""
	if status == StatusBuilding {
		col = ", started_at = " + fmt.Sprint(time.Now().Unix())
	}
	if _, err := d.db.Writer().Exec(`UPDATE deployments SET status = ?`+col+` WHERE id = ?`, status, id); err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) setBuildStats(id int64, r builder.Result) {
	if _, err := d.db.Writer().Exec(`UPDATE deployments SET build_ms = ?, build_cpu_ms = ?, peak_memory = ?, peak_swap = ? WHERE id = ?`,
		max(r.Took.Milliseconds(), 1), r.CPUTime.Milliseconds(), r.PeakMemory, r.PeakSwap, id); err != nil {
		d.log.Error("update deployment", "id", id, "err", err)
	}
}

func (d *Deployer) finish(id int64, err error) {
	status, msg := StatusSucceeded, ""
	if err != nil {
		status, msg = StatusFailed, err.Error()
	}
	if _, e := d.db.Writer().Exec(`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, msg, time.Now().Unix(), id); e != nil {
		d.log.Error("finish deployment", "id", id, "err", e)
	}
}

// Deployment returns one deployment.
func (d *Deployer) Deployment(ctx context.Context, id int64) (Deployment, error) {
	dep, err := scanDeployment(d.db.Reader().QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return dep, fmt.Errorf("deployment #%d not found", id)
	}
	return dep, err
}

// Deployments returns the newest deployments of app.
func (d *Deployer) Deployments(ctx context.Context, app string, limit int) ([]Deployment, error) {
	rows, err := d.db.Reader().QueryContext(ctx,
		`SELECT `+deploymentCols+` FROM deployments WHERE app = ? ORDER BY id DESC LIMIT ?`, app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		dep, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func (d *Deployer) insertBuild(ctx context.Context, b Build) error {
	args, _ := json.Marshal(b.Args)
	_, err := d.db.Writer().ExecContext(ctx, `INSERT INTO builds
		(app, id, git_sha, branch, subject, go_version, package, args, health_path, size, build_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app, id) DO UPDATE SET git_sha = excluded.git_sha, branch = excluded.branch,
		subject = excluded.subject, go_version = excluded.go_version, package = excluded.package,
		args = excluded.args, health_path = excluded.health_path, size = excluded.size,
		build_ms = excluded.build_ms, created_at = excluded.created_at`,
		b.App, b.ID, b.GitSHA, b.Branch, b.Subject, b.GoVersion, b.Package, string(args), b.HealthPath,
		b.Size, b.BuildTime.Milliseconds(), unix(b.CreatedAt))
	return err
}

func (d *Deployer) deleteBuild(app, id string) error {
	_, err := d.db.Writer().Exec(`DELETE FROM builds WHERE app = ? AND id = ?`, app, id)
	return err
}

// build loads one kept build.
func (d *Deployer) build(ctx context.Context, app, id string) (Build, error) {
	bs, err := d.queryBuilds(ctx, `WHERE app = ? AND id = ?`, app, id)
	if err != nil {
		return Build{}, err
	}
	if len(bs) == 0 {
		return Build{}, fmt.Errorf("build %s of %s is not kept on this server (only the last %d are)", id, app, KeepBuilds)
	}
	return bs[0], nil
}

// Builds lists the kept builds of app, newest first.
func (d *Deployer) Builds(ctx context.Context, app string) ([]Build, error) {
	bs, err := d.queryBuilds(ctx, `WHERE app = ? ORDER BY created_at DESC, id DESC`, app)
	if err != nil {
		return nil, err
	}
	cur := d.currentBuild(app)
	for i := range bs {
		bs[i].Current = bs[i].ID == cur
	}
	return bs, nil
}

func (d *Deployer) queryBuilds(ctx context.Context, where string, args ...any) ([]Build, error) {
	rows, err := d.db.Reader().QueryContext(ctx, `SELECT app, id, git_sha, branch, subject, go_version, package,
		args, health_path, size, build_ms, created_at FROM builds `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		var argv string
		var ms, created int64
		if err := rows.Scan(&b.App, &b.ID, &b.GitSHA, &b.Branch, &b.Subject, &b.GoVersion, &b.Package,
			&argv, &b.HealthPath, &b.Size, &ms, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(argv), &b.Args); err != nil {
			return nil, fmt.Errorf("build %s: bad args column: %w", b.ID, err)
		}
		b.BuildTime = time.Duration(ms) * time.Millisecond
		b.CreatedAt = fromUnix(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// DesiredRunning reports whether app should run after a goboat restart
// (default: yes).
func (d *Deployer) DesiredRunning(ctx context.Context, app string) bool {
	var s string
	err := d.db.Reader().QueryRowContext(ctx, `SELECT desired_state FROM app_state WHERE app = ?`, app).Scan(&s)
	return err != nil || s != "stopped"
}

// SetDesired records whether app should be running.
func (d *Deployer) SetDesired(ctx context.Context, app string, running bool) error {
	s := "stopped"
	if running {
		s = "running"
	}
	_, err := d.db.Writer().ExecContext(ctx, `INSERT INTO app_state (app, desired_state) VALUES (?, ?)
		ON CONFLICT (app) DO UPDATE SET desired_state = excluded.desired_state`, app, s)
	return err
}

// recover marks deployments interrupted by a goboat restart as failed.
func (d *Deployer) recoverInterrupted(ctx context.Context) (int64, error) {
	res, err := d.db.Writer().ExecContext(ctx, `UPDATE deployments
		SET status = ?, error = 'interrupted: goboat restarted during this deployment', finished_at = ?
		WHERE status IN (?, ?, ?, 'preparing')`, StatusFailed, time.Now().Unix(), StatusQueued, StatusBuilding, StatusDeploying)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
