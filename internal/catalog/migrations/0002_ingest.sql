-- Ingest (M2). The work queue is one row per (root CID, path inside it) to
-- fetch: the leaf CID is only known once the fetch resolves the path, so
-- objects keeps the state of stored leaves and its scheduling columns from
-- 0001 move here.
CREATE TABLE fetches (
    root_cid      text NOT NULL,
    subpath       text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    lane          text NOT NULL DEFAULT 'fast' CHECK (lane IN ('fast', 'slow', 'backfill')),
    creator       text NOT NULL DEFAULT '',      -- fair share key (ASA creator of the first reference)
    depth         int NOT NULL DEFAULT 0,        -- hops from a chain-level reference
    attempts      int NOT NULL DEFAULT 0,
    next_try_at   timestamptz NOT NULL DEFAULT now(),
    lease_until   timestamptz,                   -- set while a worker holds it
    leaf_cid      text,                          -- set when done
    last_error    text,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    rearmed_at    timestamptz,                   -- a failed fetch is re-armed at most once per 30 days
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (root_cid, subpath)
);
CREATE INDEX fetches_due ON fetches (lane, next_try_at) WHERE status = 'pending';
CREATE INDEX fetches_due_creator ON fetches (lane, creator, next_try_at) WHERE status = 'pending';

DROP INDEX objects_dispatch;
ALTER TABLE objects
    DROP COLUMN lane,
    DROP COLUMN creator,
    DROP COLUMN depth,
    DROP COLUMN attempts,
    DROP COLUMN next_try_at,
    DROP COLUMN lease_until;

-- Every object an asset reaches: its chain-level refs, the leaves they resolve
-- to, and the links inside JSON leaves, up to 3 hops. A function rather than a
-- view so the asset filter applies inside the recursion.
CREATE FUNCTION asset_objects(p_chain text, p_asset bigint)
RETURNS TABLE (key text, root_cid text, subpath text, leaf_cid text, depth int,
               status text, mime text, size bigint, reason text)
LANGUAGE sql STABLE AS $$
    WITH RECURSIVE walk (key, root_cid, subpath, leaf_cid, depth) AS (
        SELECT r.key, r.root_cid, r.subpath,
               CASE WHEN r.subpath = '' THEN r.root_cid ELSE a.leaf_cid END, 0
        FROM refs r
        LEFT JOIN aliases a ON a.root_cid = r.root_cid AND a.subpath = r.subpath
        WHERE r.chain = p_chain AND r.asset = p_asset
      UNION ALL
        SELECT w.key || '/' || l.json_path, l.child_cid, l.subpath,
               CASE WHEN l.subpath = '' THEN l.child_cid ELSE a.leaf_cid END, w.depth + 1
        FROM walk w
        JOIN links l ON l.parent_cid = w.leaf_cid
        LEFT JOIN aliases a ON a.root_cid = l.child_cid AND a.subpath = l.subpath
        WHERE w.depth < 3
    )
    SELECT w.key, w.root_cid, w.subpath, w.leaf_cid, w.depth, o.status, o.mime, o.size, o.reason
    FROM walk w
    LEFT JOIN objects o ON o.cid = w.leaf_cid
    ORDER BY w.depth, w.key
$$;
