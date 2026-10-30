#!/bin/sh
# Dev only: kubo as seapin's offline test-content source and stand-in sidecar
# (production runs rainbow, deploy/rainbow). Runs from /container-init.d
# before every daemon start, so the config is always ours.
# - gateway and RPC API on loopback (KUBO_GATEWAY_PORT, KUBO_API_PORT)
# - no providing, DHT client only: we fetch, we do not serve the network
# - a small blockstore with GC: seapin's bucket is the cache, not kubo
# - kubo's own gateway limits sit above seapin's lane deadlines (slow lane
#   first byte 120 s, transfer ceiling 10 min), so seapin's deadlines decide
set -e
ipfs config Addresses.API "/ip4/127.0.0.1/tcp/${KUBO_API_PORT:-5001}"
ipfs config Addresses.Gateway "/ip4/127.0.0.1/tcp/${KUBO_GATEWAY_PORT:-8080}"
ipfs config --json Addresses.Swarm "${KUBO_SWARM_ADDRESSES:-[\"/ip4/0.0.0.0/tcp/4001\", \"/ip4/0.0.0.0/udp/4001/quic-v1\"]}"
ipfs config Routing.Type autoclient
ipfs config --json Provide.Enabled false
ipfs config Datastore.StorageMax "${KUBO_STORAGE_MAX:-50GB}"
ipfs config Datastore.GCPeriod 10m
ipfs config --json Gateway.NoFetch false
ipfs config --json Gateway.DisableHTMLErrors true
ipfs config Gateway.RetrievalTimeout 150s
ipfs config Gateway.MaxRequestDuration 15m
ipfs config --json Gateway.MaxConcurrentRequests 256
ipfs config --json Swarm.ConnMgr '{"Type": "basic", "LowWater": 200, "HighWater": 400, "GracePeriod": "30s"}'
