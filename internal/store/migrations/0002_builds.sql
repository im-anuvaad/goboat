-- goboat builds apps from a branch (docs/architecture.md §7): kept builds
-- replace downloaded releases.
DROP TABLE releases;

-- Builds kept on disk (the current one plus the newest others, 3 in all).
CREATE TABLE builds (
    app         TEXT    NOT NULL,
    id          TEXT    NOT NULL,           -- first 12 hex digits of the commit
    git_sha     TEXT    NOT NULL,           -- full commit SHA
    branch      TEXT    NOT NULL,
    subject     TEXT    NOT NULL DEFAULT '', -- first line of the commit message
    go_version  TEXT    NOT NULL DEFAULT '', -- toolchain that built it
    package     TEXT    NOT NULL,           -- from goboat.toml
    args        TEXT    NOT NULL,           -- JSON array, from goboat.toml
    health_path TEXT    NOT NULL,
    size        INTEGER NOT NULL DEFAULT 0, -- of the binary
    build_ms    INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (app, id)
) STRICT;

-- The branch an app is built from.
ALTER TABLE apps ADD COLUMN branch TEXT NOT NULL DEFAULT '';

ALTER TABLE deployments RENAME COLUMN release_id TO build_id;
ALTER TABLE deployments ADD COLUMN branch TEXT NOT NULL DEFAULT '';

ALTER TABLE backups RENAME COLUMN release_id TO git_sha;
