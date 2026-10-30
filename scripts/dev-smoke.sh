#!/bin/sh
# M1 exit check against the running dev stack (docker compose up -d): seeds
# objects through `seapin seed`, plants one in the bucket behind the policy's
# back, and checks what the gateway answers. Stops at the first mismatch.
set -eu
cd "$(dirname "$0")/.."
. ./.env
GW=http://127.0.0.1:21090
FILER=http://127.0.0.1:21888

rawcid() { # CIDv1, raw codec, sha2-256, base32 — what `ipfs add --raw-leaves` gives a one-block file
	python3 -I -c 'import sys,hashlib,base64; d=hashlib.sha256(open(sys.argv[1],"rb").read()).digest(); print("b"+base64.b32encode(bytes([1,0x55,0x12,0x20])+d).decode().lower().rstrip("="))' "$1"
}
objkey() { n=${#1}; echo "$(printf %s "$1" | cut -c$((n-3))-$((n-2)))/$(printf %s "$1" | cut -c$((n-1))-$n)/$1"; }
fail() { echo "FAIL: $*"; exit 1; }
expect() { # expect <code> <curl args...>
	want=$1; shift
	got=$(curl -s -o /dev/null -w '%{http_code}' "$@")
	[ "$got" = "$want" ] || fail "$* -> $got, want $want"
	echo "ok   $want  $*"
}
psql_dev() { docker compose exec -T -e PGPASSWORD="$PG_PASSWORD" postgres psql -h 127.0.0.1 -p 21432 -U seapin -d seapin -qAtc "$1"; }
restart_serve() {
	docker compose restart seapin >/dev/null
	for _ in $(seq 30); do curl -sf -o /dev/null "$GW/health" && return; sleep 1; done
	fail "seapin not healthy after restart"
}

python3 -I - dev/seed/pixel.png <<'EOF'
import struct, sys, zlib
def chunk(t, d): return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d))
png = (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 6, 0, 0, 0))
       + chunk(b"IDAT", zlib.compress(b"\x00\xff\x00\x00\xff")) + chunk(b"IEND", b""))
open(sys.argv[1], "wb").write(png)
EOF
printf '<!DOCTYPE html><html><script>alert(1)</script></html>' > dev/seed/page.html
printf '<html><script>alert(2)</script></html>' > dev/seed/planted.html
head -c 64 /dev/urandom > dev/seed/never-seeded.bin
PNG=$(rawcid dev/seed/pixel.png)
HTML=$(rawcid dev/seed/page.html)
PLANT=$(rawcid dev/seed/planted.html)
MISSING=$(rawcid dev/seed/never-seeded.bin)

docker compose run --rm -T seapin-tool seed "$PNG" /seed/pixel.png 2>&1 | tail -1
if docker compose run --rm -T seapin-tool seed "$HTML" /seed/page.html >/dev/null 2>&1; then
	fail "seed accepted HTML"
fi
echo "ok   seed refused HTML"
curl -sf -o /dev/null -F "file=@dev/seed/planted.html;type=text/html" "$FILER/buckets/ipfs/$(objkey "$PLANT")" ||
	fail "could not plant an object through the filer"

expect 200 "$GW/ipfs/$PNG"
expect 206 -r 0-7 "$GW/ipfs/$PNG"
expect 304 -H "If-None-Match: \"$PNG\"" "$GW/ipfs/$PNG"
expect 200 -I "$GW/ipfs/$PNG"
expect 200 "$GW/ipfs/$PNG/"
expect 403 "$GW/ipfs/$HTML"
expect 403 "$GW/ipfs/$PLANT"
expect 404 "$GW/ipfs/$MISSING"
expect 404 "$GW/ipfs/$PNG/sub/path.png"
expect 400 "$GW/ipfs/not-a-cid"
expect 405 -X POST "$GW/ipfs/$PNG"
expect 404 -X POST "$GW/upload"

[ "$(curl -s -r 0-7 "$GW/ipfs/$PNG" | od -An -tx1 | tr -d ' \n')" = "89504e470d0a1a0a" ] || fail "range bytes"
echo "ok   range bytes are the PNG signature"
hdrs=$(curl -s -D - -o /dev/null "$GW/ipfs/$PNG" | tr -d '\r')
for h in "content-type: image/png" "x-content-type-options: nosniff" "etag: \"$PNG\"" "cache-control: public, max-age=29030400, immutable"; do
	printf '%s\n' "$hdrs" | grep -qi "^$h\$" || fail "missing header: $h"
done
printf '%s\n' "$hdrs" | grep -qi '^content-security-policy: .*sandbox' || fail "missing sandbox CSP"
echo "ok   response headers"
miss_cc=$(curl -s -D - -o /dev/null "$GW/ipfs/$MISSING" | tr -d '\r' | grep -i '^cache-control:')
[ "$miss_cc" = "Cache-Control: public, max-age=60" ] || fail "404 cache-control: $miss_cc"
echo "ok   404 is cacheable for 60 s only"

# The serve identity is read-only.
code=$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 "aws:amz:us-east-1:s3" \
	--user "$SERVE_S3_ACCESS_KEY:$SERVE_S3_SECRET_KEY" -X PUT --data-binary x \
	"http://127.0.0.1:21833/ipfs/$(objkey "$MISSING")")
[ "$code" = "403" ] || fail "serve identity PUT -> $code, want 403"
echo "ok   serve identity cannot write (403)"

psql_dev "INSERT INTO takedowns (cid, reason) VALUES ('$PNG', 'smoke test') ON CONFLICT DO NOTHING"
restart_serve
expect 451 "$GW/ipfs/$PNG"
psql_dev "DELETE FROM takedowns WHERE cid = '$PNG' AND reason = 'smoke test'"
restart_serve
expect 200 "$GW/ipfs/$PNG"

[ "$(psql_dev "SELECT status || ' ' || reason FROM objects WHERE cid = '$HTML'")" = "blocked not_allowed:text/html; charset=utf-8" ] ||
	fail "catalog row for the refused HTML"
echo "ok   catalog records the refusal"
curl -s "$GW/metrics" | grep -q '^seapin_serve_requests_total{outcome="hit"}' || fail "metrics"
echo "ok   metrics"
echo "PASS"
