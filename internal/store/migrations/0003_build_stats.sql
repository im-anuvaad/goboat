-- What a deployment's build used (docs/architecture.md §7.3), shown on the
-- deployment page. 0 for rollbacks and kept builds.
ALTER TABLE deployments ADD COLUMN build_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN build_cpu_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN peak_memory INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN peak_swap INTEGER NOT NULL DEFAULT 0;
