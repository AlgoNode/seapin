#!/bin/sh
# M2 check against the running dev stack: adds content to the dev kubo,
# publishes references to the "seapin" exchange, and checks what ingest made
# of them through the gateway, the catalog, the internal listener and the
# event stream. Stops at the first mismatch.
set -eu
cd "$(dirname "$0")/.."
. ./.env
GW=http://127.0.0.1:21090
INT=http://127.0.0.1:21092
MQ=http://127.0.0.1:21673
METRICS=http://127.0.0.1:21091/metrics
CHAIN=mainnet-v1.0
RUN=$(date +%s)   # fresh asset ids per run
A_JSON=$((RUN * 10 + 1)) A_HTML=$((RUN * 10 + 2)) A_MISSING=$((RUN * 10 + 3))

fail() { echo "FAIL: $*"; exit 1; }
ok() { echo "ok   $*"; }
kadd() { docker compose exec -T kubo ipfs add -Q --cid-version=1 "$@"; }
psql_dev() { docker compose exec -T -e PGPASSWORD="$PG_PASSWORD" postgres psql -h 127.0.0.1 -p 21432 -U seapin -d seapin -qAtc "$1"; }
mq() { curl -sf -u "seapin:$RMQ_PASSWORD" -H 'content-type: application/json' "$@"; }
publish() {
	body=$(python3 -I -c 'import json,sys; print(json.dumps({"properties": {"content_type": "application/json", "delivery_mode": 2}, "routing_key": "", "payload": sys.argv[1], "payload_encoding": "string"}))' "$1")
	mq -X POST "$MQ/api/exchanges/%2F/seapin/publish" -d "$body" | grep -q '"routed":true' || fail "publish not routed"
}
ref() { # ref <asset> <cid> [subpath]
	python3 -I -c 'import json,sys; a=int(sys.argv[2]); print(json.dumps({"v": 1, "type": "ref", "key": "%s/%d/url" % (sys.argv[1], a), "chain": sys.argv[1], "asset": a, "path": "url", "cid": sys.argv[3], "subpath": sys.argv[4], "creator": "SMOKECREATOR", "src": "live", "round": 1000, "txid": "SMOKE", "block_time": "2026-10-30T12:00:00Z"}))' "$CHAIN" "$1" "$2" "${3:-}"
}
wait_code() { # wait_code <code> <url>: poll up to 30 s
	for _ in $(seq 60); do
		[ "$(curl -s -o /dev/null -w '%{http_code}' "$2")" = "$1" ] && { ok "$1  $2"; return; }
		sleep 0.5
	done
	fail "$2 never answered $1 (last $(curl -s -o /dev/null -w '%{http_code}' "$2"))"
}
metric() { curl -s "$METRICS" | awk -v m="$1" '$1 == m {print $2}' | head -1; }

# Content: an image, an HTML page, a directory with an image, and metadata JSON
# pointing at the image and into the directory.
S=dev/seed/ingest-$RUN
mkdir -p "$S/dir"
python3 -I - "$S/a.png" "$S/dir/1.png" <<'EOF'
import struct, sys, zlib
def chunk(t, d): return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d))
for i, path in enumerate(sys.argv[1:]):
    raw = b"\x00" + bytes([255, i * 80, 0, 255])
    png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 6, 0, 0, 0))
           + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"tEXt", b"run\x00" + path.encode()) + chunk(b"IEND", b""))
    open(path, "wb").write(png)
EOF
printf '<!DOCTYPE html><html><script>alert(1)</script></html>%s' "$RUN" > "$S/page.html"
head -c 64 /dev/urandom > "$S/never-added.bin"
C=/seed/ingest-$RUN
PNG=$(kadd "$C/a.png")
DIR=$(kadd -r "$C/dir")
HTML=$(kadd "$C/page.html")
printf '{"name":"Smoke %s","image":"ipfs://%s","properties":{"file_url":"ipfs://%s/1.png"}}' "$RUN" "$PNG" "$DIR" > "$S/meta.json"
JSON=$(kadd "$C/meta.json")
MISSING=$(python3 -I -c 'import sys,hashlib,base64; d=hashlib.sha256(open(sys.argv[1],"rb").read()).digest(); print("b"+base64.b32encode(bytes([1,0x55,0x12,0x20])+d).decode().lower().rstrip("="))' "$S/never-added.bin")
ok "kubo has json $JSON, png $PNG, dir $DIR, html $HTML"

# Watch the event stream.
EVQ=smoke-events-$RUN
mq -X PUT "$MQ/api/exchanges/%2F/seapin.events" -d '{"type":"topic","durable":true}' >/dev/null
mq -X PUT "$MQ/api/queues/%2F/$EVQ" -d '{"durable":true,"auto_delete":false}' >/dev/null  # RabbitMQ 4 refuses transient non-exclusive queues
mq -X POST "$MQ/api/bindings/%2F/e/seapin.events/q/$EVQ" -d '{"routing_key":"#"}' >/dev/null

publish "$(ref "$A_JSON" "$JSON")"
publish "$(ref "$A_HTML" "$HTML")"
publish "$(ref "$A_MISSING" "$MISSING")"

wait_code 200 "$GW/ipfs/$JSON"
wait_code 200 "$GW/ipfs/$PNG"
wait_code 200 "$GW/ipfs/$DIR/1.png"
wait_code 403 "$GW/ipfs/$HTML"
[ "$(curl -s -o /dev/null -w '%{content_type}' "$GW/ipfs/$JSON")" = "application/json" ] || fail "JSON content type"
ok "metadata JSON, its image and the file inside the directory are cached; HTML refused"

[ "$(curl -s -o /dev/null -w '%{http_code}' "$GW/ipfs/$MISSING")" = "404" ] || fail "missing CID is not 404"
for _ in $(seq 30); do
	row=$(psql_dev "SELECT lane || ' ' || attempts || ' ' || (next_try_at > now() + interval '4 minutes') FROM fetches WHERE root_cid = '$MISSING'")
	[ "$row" = "slow 1 true" ] && break
	sleep 0.5
done
[ "$row" = "slow 1 true" ] || fail "unfetchable CID: fetches row is '$row', want 'slow 1 true'"
ok "unfetchable CID moved to the slow lane, next try in 5 min"

objs=$(curl -sf -H "Authorization: Bearer $INTERNAL_TOKEN" "$INT/internal/assets/$CHAIN/$A_JSON")
printf '%s' "$objs" | python3 -I -c '
import json, sys
d = json.load(sys.stdin)
got = {o["key"].split("/", 2)[2]: (o.get("status"), o.get("mime")) for o in d["objects"]}
want = {"url": ("cached", "application/json"), "url/image": ("cached", "image/png"),
        "url/properties/file_url": ("cached", "image/png")}
assert got == want, got
assert d["objects"][0]["content"]["name"].startswith("Smoke"), d["objects"][0]
' || fail "internal assets: $objs"
ok "/internal/assets lists the 3 objects with keys, types and the JSON content"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$INT/internal/assets/$CHAIN/$A_JSON")" = "401" ] || fail "internal listener without token"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $INTERNAL_TOKEN" "$INT/internal/ipfs/$PNG")" = "200" ] || fail "internal ipfs"
ok "internal listener: 401 without token, /internal/ipfs serves with it"

cached=$(metric 'seapin_ingest_fetches_total{lane="fast",outcome="cached"}')
fetches=$(psql_dev "SELECT count(*) FROM fetches")
publish "$(ref "$A_JSON" "$JSON")"
publish "$(ref "$A_HTML" "$HTML")"
sleep 3
[ "$(metric 'seapin_ingest_fetches_total{lane="fast",outcome="cached"}')" = "$cached" ] || fail "re-published refs were fetched again"
[ "$(psql_dev "SELECT count(*) FROM fetches")" = "$fetches" ] || fail "re-published refs created fetches"
ok "re-published refs cost no fetch"

events=$(mq -X POST "$MQ/api/queues/%2F/$EVQ/get" -d '{"count":50,"ackmode":"ack_requeue_false","encoding":"auto"}')
mq -X DELETE "$MQ/api/queues/%2F/$EVQ" >/dev/null
printf '%s' "$events" | python3 -I -c '
import json, sys
evs = [json.loads(m["payload"]) for m in json.load(sys.stdin)]
st = sorted((e["status"], e["mime"]) for e in evs if e["status"] in ("cached", "blocked"))
assert st == [("blocked", "text/html; charset=utf-8"), ("cached", "application/json"), ("cached", "image/png"), ("cached", "image/png")], st
assert all(e["region"] == "dev" and e["v"] == 1 for e in evs), evs
' || fail "events: $events"
ok "events: 3 cached + 1 blocked on seapin.events"
echo PASS
