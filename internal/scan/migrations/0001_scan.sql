-- seapin-scan state (PLAN §1 Scanner): the two block cursors and what the
-- scanner knows about each asset.

CREATE TABLE scan_state (
    chain         text PRIMARY KEY,
    bootstrap     bigint NOT NULL,  -- tip at the first start (T0)
    live_next     bigint NOT NULL,  -- next round the live cursor reads (ascending)
    backfill_next bigint NOT NULL,  -- next round the backfill reads (descending); below floor = done
    floor         bigint NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- One row per asset the scanner has seen a configuration of. The backfill
-- walks down, so the first configuration it meets is the newest; older ones
-- are skipped (ordered by sequence: one block may hold several). The URL is
-- only in the creation transaction, so a newer reconfiguration's addresses
-- wait here until the creation is reached.
CREATE TABLE scan_assets (
    chain        text NOT NULL,
    asset        bigint NOT NULL,
    newest_seq   bigint NOT NULL,   -- (round << 20) | position in the block of the newest configuration
    destroyed    boolean NOT NULL DEFAULT false,
    url          text,
    name         text,
    unit         text,
    creator      text,
    manager_addr bytea,
    reserve_addr bytea,
    freeze_addr bytea,
    clawback_addr bytea,
    ref_round    bigint,            -- round of the newest refs sent for this asset
    meta_round   bigint,            -- ref_round x_asset_meta was last written for
    meta_tries   int NOT NULL DEFAULT 0,
    meta_next_at timestamptz,
    PRIMARY KEY (chain, asset)
);
CREATE INDEX scan_assets_meta_due ON scan_assets (meta_next_at)
    WHERE ref_round IS NOT NULL AND meta_round IS DISTINCT FROM ref_round;
