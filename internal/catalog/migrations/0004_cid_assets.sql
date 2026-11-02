-- CID -> assets: the reverse of asset_objects (2026-11-02). Walks up from a
-- CID through directory paths (aliases) and JSON links (up to 3 hops) to the
-- chain references, returning each asset with the same key asset_objects
-- gives the object (<chain>/<asset>/<path>[/<json path>...]).
CREATE INDEX IF NOT EXISTS aliases_leaf ON aliases (leaf_cid);

CREATE OR REPLACE FUNCTION cid_assets(p_chain text, p_cid text, p_limit int)
RETURNS TABLE (asset bigint, key text, depth int)
LANGUAGE sql STABLE AS $$
    WITH RECURSIVE node (cid, suffix, depth) AS (
        SELECT p_cid, ''::text, 0
      UNION ALL
        SELECT l.parent_cid, '/' || l.json_path || n.suffix, n.depth + 1
        FROM node n
        CROSS JOIN LATERAL (
            SELECT n.cid AS root_cid, ''::text AS subpath
            UNION ALL SELECT a.root_cid, a.subpath FROM aliases a WHERE a.leaf_cid = n.cid
        ) p
        JOIN links l ON l.child_cid = p.root_cid AND l.subpath = p.subpath
        WHERE n.depth < 3
    )
    SELECT DISTINCT r.asset, r.key || n.suffix, n.depth
    FROM node n
    CROSS JOIN LATERAL (
        SELECT n.cid AS root_cid, ''::text AS subpath
        UNION ALL SELECT a.root_cid, a.subpath FROM aliases a WHERE a.leaf_cid = n.cid
    ) p
    JOIN refs r ON r.root_cid = p.root_cid AND r.subpath = p.subpath AND r.chain = p_chain
    ORDER BY 3, 1, 2
    LIMIT p_limit
$$;
