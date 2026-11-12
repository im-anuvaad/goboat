-- Cloudflare objects goboat no longer uses, kept until Cloudflare confirms
-- they are gone (docs/architecture.md §5.6).
--   host:     a hostname goboat stopped serving: its A/AAAA records that
--             point at this server (detail = the addresses it had then) and
--             every Origin CA certificate for exactly that hostname
--   aop_cert: an uploaded zone-level AOP client certificate (object_id)
--   zone:     a zone without goboat hostnames: delete goboat's AOP
--             certificates and turn zone-level AOP off
CREATE TABLE cf_cleanup (
    id         INTEGER PRIMARY KEY,
    kind       TEXT    NOT NULL,
    zone_id    TEXT    NOT NULL DEFAULT '',
    host       TEXT    NOT NULL DEFAULT '',
    object_id  TEXT    NOT NULL DEFAULT '',
    detail     TEXT    NOT NULL DEFAULT '',
    reason     TEXT    NOT NULL,
    attempts   INTEGER NOT NULL DEFAULT 0,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_try   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (kind, zone_id, host, object_id)
) STRICT;
