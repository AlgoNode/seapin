-- Creators whose references are kept but whose content is never fetched (operator 2026-11-03). Their root CIDs are
-- recorded as blocked objects, so serve answers 403 at once (cacheable) without asking the peer or starting a
-- demand fetch, and ingest never queues them (any objects row makes a root "known").
CREATE TABLE IF NOT EXISTS blocked_creators (
    creator  text PRIMARY KEY,
    reason   text NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now()
);

-- "Digital signed document" (unit DSD): 9,601 assets since 2022-12, still minting; the content is signed PDFs (not
-- on the policy allowlist) or never published at all. 3,175 of the 5,744 live-asset CIDs no source had (2026-11-03).
INSERT INTO blocked_creators (creator, reason) VALUES
    ('YKK3HOD3T2FFWLBRXZUTQHLAZBXGS2CK73U3NSBQOOVEUL2HU7GV7YFOAE', 'signed documents (DSD), not NFT media')
ON CONFLICT (creator) DO NOTHING;

-- Settle what is already there: block the roots (never downgrading a cached object) and close their fetches.
INSERT INTO objects (cid, status, reason)
SELECT DISTINCT r.root_cid, 'blocked', 'creator_excluded: ' || b.reason
FROM refs r JOIN blocked_creators b ON b.creator = r.creator
ON CONFLICT (cid) DO UPDATE SET status = 'blocked', reason = EXCLUDED.reason
WHERE objects.status NOT IN ('cached', 'blocked');

UPDATE fetches f SET status = 'done', leaf_cid = f.root_cid, lease_until = NULL,
       last_error = 'creator_excluded', updated_at = now()
WHERE f.status <> 'done' AND f.subpath = ''
  AND EXISTS (SELECT 1 FROM objects o WHERE o.cid = f.root_cid AND o.status = 'blocked'
              AND o.reason LIKE 'creator_excluded:%');
