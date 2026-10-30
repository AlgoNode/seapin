# rainbow: seapin's IPFS gateway sidecar (D1)

seapin's ingest fetches through [rainbow](https://github.com/ipfs/rainbow) (`SIDECAR_URL`); serve never talks
to it. These are the flags the M0 spike ran with (PLAN §3 M0, 2026-10-30):
- 95% of recent CIDs fetched, with first byte p50 0.64 s
- 343 MiB peak RSS
- the same `X-Ipfs-Roots`, ETag and "no link named" 404 behaviour ingest relies on

```
ghcr.io/ipfs/rainbow:v1.24.1        # newest image on GHCR as of 2026-10-30 (v1.25.0 has no image yet)
  --datadir /data
  --gateway-listen-address 127.0.0.1:8090
  --ctl-listen-address 127.0.0.1:8091                                     # metrics
  --libp2p-listen-addrs /ip4/127.0.0.1/tcp/4001,/ip4/127.0.0.1/udp/4001/quic-v1   # outbound only
  --dht-routing standard                 # "accelerated" finds providers faster but crawls the whole DHT: try in M4
  --retrieval-timeout 150s               # above seapin's slow-lane first-byte deadline (120 s): seapin decides
  --max-request-duration 15m             # above seapin's transfer ceiling (10 min)
  --gc-interval 1h                       # the bucket is the cache; rainbow's blockstore is scratch
```

Run it as uid 1000 with a read-only root, `cap_drop: [ALL]`, `/tmp` on tmpfs and a `mem_limit` of about 1–2 GB.
Fleet compose conventions apply (nodely-deploy); the M4 compose is written then.
