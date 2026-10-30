#!/bin/sh
# Prepares the dev stack: data dirs, .env with fresh random credentials, and the
# SeaweedFS S3 identities file rendered from it. Safe to re-run: an existing
# .env is kept and dev/s3.json is re-rendered from it.
set -eu
cd "$(dirname "$0")/.."
umask 077

mkdir -p dev/seaweed dev/postgres dev/seed dev/rabbitmq dev/kubo

rand() { od -An -tx1 -N"$1" /dev/urandom | tr -d ' \n'; }

if [ ! -f .env ]; then
	cat > .env <<EOF
PG_PASSWORD=$(rand 24)
SERVE_S3_ACCESS_KEY=serve$(rand 8)
SERVE_S3_SECRET_KEY=$(rand 24)
INGEST_S3_ACCESS_KEY=ingest$(rand 8)
INGEST_S3_SECRET_KEY=$(rand 24)
EOF
	echo "wrote .env"
fi
# Secrets added after the first version of this script.
grep -q '^RMQ_PASSWORD=' .env || echo "RMQ_PASSWORD=$(rand 24)" >> .env
grep -q '^INTERNAL_TOKEN=' .env || echo "INTERNAL_TOKEN=$(rand 24)" >> .env
. ./.env

# serve may only read; ingest (and migrate/seed) also writes and creates the bucket.
cat > dev/s3.json <<EOF
{
  "identities": [
    {
      "name": "seapin-serve",
      "credentials": [{"accessKey": "$SERVE_S3_ACCESS_KEY", "secretKey": "$SERVE_S3_SECRET_KEY"}],
      "actions": ["Read:ipfs"]
    },
    {
      "name": "seapin-ingest",
      "credentials": [{"accessKey": "$INGEST_S3_ACCESS_KEY", "secretKey": "$INGEST_S3_SECRET_KEY"}],
      "actions": ["Read:ipfs", "Write:ipfs", "List:ipfs", "Admin:ipfs"]
    }
  ]
}
EOF
echo "wrote dev/s3.json"
