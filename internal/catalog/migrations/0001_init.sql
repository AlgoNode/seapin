-- seapin catalog (PLAN.md §1 Catalog). Append-only: rows change state but are
-- never deleted.

-- One row per leaf CID (CIDv1 base32): what it is and where it is in ingest.
CREATE TABLE objects (
    cid           text PRIMARY KEY,
    status        text NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending', 'cached', 'blocked', 'failed')),
    mime          text,           -- served Content-Type when cached, detected type when blocked
    size          bigint,
    reason        text,           -- not_allowed:<mime> | too_large | directory | ...
    first_seen_at timestamptz NOT NULL DEFAULT now(),  -- block time of the first reference ("upload date")
    cached_at     timestamptz,
    source        text,           -- kubo | peer | pinata | seed
    lane          text NOT NULL DEFAULT 'fast' CHECK (lane IN ('fast', 'slow', 'backfill')),
    creator       text,           -- creator of the first reference, for fair scheduling
    depth         int NOT NULL DEFAULT 0,             -- hops from a chain-level reference
    attempts      int NOT NULL DEFAULT 0,
    next_try_at   timestamptz NOT NULL DEFAULT now(),
    lease_until   timestamptz
);
CREATE INDEX objects_dispatch ON objects (lane, next_try_at) WHERE status = 'pending';

-- Chain-level references: '<chain>/<asset>/url', '<chain>/<asset>/note/media_url'.
CREATE TABLE refs (
    key        text PRIMARY KEY,
    chain      text NOT NULL,
    asset      bigint NOT NULL,
    path       text NOT NULL,
    root_cid   text NOT NULL,
    subpath    text NOT NULL DEFAULT '',
    creator    text,
    round      bigint NOT NULL,
    txid       text,
    block_time timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX refs_root_cid ON refs (root_cid);
CREATE INDEX refs_asset ON refs (chain, asset);

-- IPFS links found inside a JSON object; a property of the parent CID.
CREATE TABLE links (
    parent_cid text NOT NULL,
    json_path  text NOT NULL,
    child_cid  text NOT NULL,
    subpath    text NOT NULL DEFAULT '',
    PRIMARY KEY (parent_cid, json_path)
);
CREATE INDEX links_child_cid ON links (child_cid);

-- A path inside a UnixFS directory CID, resolved to the leaf CID stored for it.
CREATE TABLE aliases (
    root_cid text NOT NULL,
    subpath  text NOT NULL,
    leaf_cid text NOT NULL,
    PRIMARY KEY (root_cid, subpath)
);

-- Operator takedowns: served as 451 even when the object is stored.
CREATE TABLE takedowns (
    cid        text PRIMARY KEY,
    reason     text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
