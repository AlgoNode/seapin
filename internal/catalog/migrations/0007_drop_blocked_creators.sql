-- Withdraw 0006 (operator 2026-11-03): not every asset of a creator has the same forbidden content, so blocking stays
-- per object, decided by its bytes (policy) as before. The creator_excluded rows held no bytes and no evidence about
-- the object: delete them, re-queue their fetches as backfill so each CID is judged on what it is, drop the table.
UPDATE fetches SET status = 'pending', lane = 'backfill', attempts = 0, next_try_at = now(), rearmed_at = now(),
       leaf_cid = NULL, lease_until = NULL, last_error = NULL, updated_at = now()
WHERE last_error = 'creator_excluded';

DELETE FROM objects WHERE status = 'blocked' AND reason LIKE 'creator_excluded:%';

DROP TABLE IF EXISTS blocked_creators;
