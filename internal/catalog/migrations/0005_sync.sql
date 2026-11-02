-- Anti-entropy between regions (PLAN §2, M5; 2026-11-02): each region pages
-- through the peer's catalog since a watermark and queues what it lacks.
CREATE INDEX IF NOT EXISTS refs_sync ON refs (updated_at, key);
CREATE INDEX IF NOT EXISTS objects_sync ON objects (cached_at, cid) WHERE status = 'cached';

-- Where the last pull from the peer stopped, per kind (refs, objects, aliases).
CREATE TABLE IF NOT EXISTS peer_sync (
    kind       text PRIMARY KEY,
    since      timestamptz NOT NULL DEFAULT '1970-01-01',
    after_key  text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
