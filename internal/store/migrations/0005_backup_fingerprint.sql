-- What the databases looked like when a backup was taken (size and mtime of
-- each database and its WAL); a scheduled backup is skipped when nothing
-- changed since (docs/architecture.md §8).
ALTER TABLE backups ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';
