-- Read-through for whitelisted referers (D2 revised 2026-10-30): a miss on a
-- CID no ASA references queues a fetch in its own lane, fair per referring site.
ALTER TABLE fetches DROP CONSTRAINT fetches_lane_check;
ALTER TABLE fetches ADD CONSTRAINT fetches_lane_check
    CHECK (lane IN ('fast', 'slow', 'backfill', 'demand'));
