# Seapin

Anonymous IPFS HTTP gateway backed by S3-compatible storage.

Stores and serves content-addressed objects at `/ipfs/{CID}` routes. Uploads compute a CIDv1 (raw codec, SHA2-256) and store the file in an S3 bucket keyed by CID.

## Quick Start

```bash
docker compose up -d
```

This starts:
- **MinIO** on ports 9000 (API) and 9001 (console)
- **Seapin** on port 8080

The S3 bucket is created automatically on startup if it doesn't exist.

## Upload

```bash
curl -X POST -F file=@myfile.txt http://localhost:8080/upload
```

Response:

```json
{"cid":"bafkreig5z3h...","url":"/ipfs/bafkreig5z3h..."}
```

## Download

```bash
curl http://localhost:8080/ipfs/bafkreig5z3h...
```

## Configuration

All settings are via environment variables (with defaults for the docker-compose setup):

| Variable | Default | Description |
|---|---|---|
| `S3_ENDPOINT` | `minio:9000` | S3-compatible endpoint |
| `S3_BUCKET` | `ipfs` | Bucket name |
| `S3_ACCESS_KEY` | `minioadmin` | Access key |
| `S3_SECRET_KEY` | `minioadmin` | Secret key |
| `S3_USE_SSL` | `false` | Use HTTPS for S3 |
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
