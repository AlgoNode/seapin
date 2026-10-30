-- Dev stand-in for the etl Postgres table Nodely's GraphQL reads (algocfg indexer graphql.sql).
CREATE TABLE IF NOT EXISTS public.x_asset_meta (
    asset      int8 NOT NULL PRIMARY KEY,
    meta       jsonb,
    updated_at timestamp,
    cached_at  timestamp
);
