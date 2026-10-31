#!/usr/bin/env python3
"""Shadow comparison (PLAN M4): replay a sample of real gateway requests against the current gateway and seapin,
and compare status, Content-Type, length and body hash. Stdlib only; read-only on both sides.

  python3 -I scripts/shadow-compare.py [-n 500] [--hours 24] [--sample FILE] [--whitelisted] [--seapin URL]

Sample   distinct /ipfs/<cid>[/path] paths from a random, request-weighted draw of nodely.apilog on dw2 (api 'ipfs',
         public hosts, the last --hours; query strings such as Bunny's optimizer dropped). Needs a ch-dw2 client
         ($SHADOW_CH, else ch-dw2.sh on PATH, else ~/ch-dw2.sh). --sample FILE replays an earlier sample.tsv, so a
         later run measures the same requests.
Old      GET <--old><path>?seapin-shadow=<random> with Referer $SHADOW_REFERER (a whitelisted referer; from the
         environment or the repo's .env, never in git). The cache-buster makes the CDN ask the origin, so the answer
         is the old gateway's own and not a copy cached for another referer.
seapin   GET <path> through an ssh tunnel to 127.0.0.1:8095 on the region host (--ssh-host, --ssh-jump), or --seapin
         URL. Without --whitelisted no X-Seapin-Whitelisted header is sent: a miss is a plain 404 and nothing is
         fetched. With it, requests whose logged status was not 403 carry the header (as openresty will) and a miss
         is read through.
Referenced  the root CID is in the x_asset_meta extract (--refset, gzip TSV, CID in column 3; M0) or in seapin's
         catalog (refs, links; one read-only psql over ssh).

Classes (one per path):
  identical             both 2xx, same bytes and Content-Type
  type-differs          both 2xx, same bytes, different Content-Type
  bytes-differ          both 2xx, different bytes
  seapin-blocked        old 2xx, seapin 403 (policy: a type seapin does not serve, a directory, too large)
  missing-referenced    old 2xx, seapin 404, root CID referenced (not cached yet)
  missing-unreferenced  old 2xx, seapin 404, root CID referenced nowhere (D2: 404 unless whitelisted read-through)
  seapin-error          old 2xx, seapin anything else (5xx, 504 read-through timeout, connection error)
  takedown              seapin 451
  old-fail-seapin-ok    old not 2xx, seapin 2xx
  both-fail             neither 2xx
Leak: a seapin 2xx whose Content-Type is not in internal/policy's allowlist, or without nosniff and a sandbox CSP.

Output: <out>/<UTC time>/{sample.tsv,results.tsv,summary.txt}; <out> is $SHADOW_OUT or ~/dev/tmp/seapin-shadow.
"""
import argparse
import base64
import binascii
import collections
import concurrent.futures as cf
import datetime as dt
import gzip
import hashlib
import os
import re
import secrets
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# internal/policy: the Content-Types seapin stores and serves. Keep in sync.
ALLOWED = {
    "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp", "image/svg+xml",
    "video/mp4", "video/webm", "video/quicktime",
    "audio/mpeg", "audio/ogg", "audio/wav", "audio/flac", "audio/aac", "audio/mp4",
    "model/gltf-binary", "application/json",
}

CID = r"(Qm[1-9A-HJ-NP-Za-km-z]{44}|b[a-z2-7]{50,}|B[A-Z2-7]{50,}|z[1-9A-HJ-NP-Za-km-z]{40,}|k[0-9a-z]{40,}|f[0-9a-f]{60,})"
PATH_RE = re.compile(r"^/ipfs/" + CID + r"(/[^?#]*)?")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SSH_OPTS = ["-o", "BatchMode=yes", "-o", "ConnectTimeout=15"]
MAX_CONCURRENCY = 8  # gentle on the old gateway

# --- CIDs (stdlib): any multibase CID -> CIDv1 base32 lower, as cidkey.Normalize ---------------------------------

B58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
B58I = {c: i for i, c in enumerate(B58)}


def _b58dec(s):
    n = 0
    for ch in s:
        n = n * 58 + B58I[ch]
    raw = n.to_bytes((n.bit_length() + 7) // 8, "big") if n else b""
    return b"\x00" * (len(s) - len(s.lstrip("1"))) + raw


def _b58enc(b):
    n, out = int.from_bytes(b, "big"), ""
    while n:
        n, r = divmod(n, 58)
        out = B58[r] + out
    return "1" * (len(b) - len(b.lstrip(b"\x00"))) + out


def cid_bytes(s):
    if len(s) == 46 and s.startswith("Qm"):
        return bytes([1, 0x70]) + _b58dec(s)
    p, body = s[0], s[1:]
    if p in "bB":
        u = body.upper()
        raw = base64.b32decode(u + "=" * (-len(u) % 8))
    elif p == "z":
        raw = _b58dec(body)
    elif p in "fF":
        raw = bytes.fromhex(body)
    elif p in "kK":
        n = int(body.lower(), 36)
        raw = n.to_bytes((n.bit_length() + 7) // 8, "big")
    else:
        raise ValueError("multibase")
    if not raw or raw[0] != 1:
        raise ValueError("version")
    return raw


def normalize(s):
    try:
        return "b" + base64.b32encode(cid_bytes(s)).decode().rstrip("=").lower()
    except (ValueError, KeyError, binascii.Error, OverflowError):
        return None


def forms(s):
    """The spellings x_asset_meta may hold for CID s: v1 base32 and, for dag-pb sha2-256, v0."""
    raw = cid_bytes(s)
    out = {"b" + base64.b32encode(raw).decode().rstrip("=").lower(), s}
    if raw[1] == 0x70 and raw[2:4] == b"\x12\x20" and len(raw) == 36:
        out.add(_b58enc(raw[2:]))
    return out


# --- sample --------------------------------------------------------------------------------------------------------

def ch_client():
    c = os.environ.get("SHADOW_CH") or shutil.which("ch-dw2.sh") or os.path.expanduser("~/ch-dw2.sh")
    if not os.path.exists(c):
        sys.exit("no ClickHouse dw2 client: set SHADOW_CH")
    return c


def draw_sample(n, hours):
    sql = f"""SELECT message, code, ld, ref, host FROM nodely.apilog
WHERE ts >= now() - INTERVAL {int(hours)} HOUR AND api = 'ipfs' AND m IN ('GET', 'HEAD')
  AND host NOT ILIKE '%nobunny%' AND host NOT LIKE '%.b-cdn.net' AND match(message, '^/ipfs/[QbBzkf]')
ORDER BY rand() LIMIT {int(n) * 4}
SETTINGS readonly=2, max_threads=4, max_execution_time=120, log_comment='seapin-shadow' FORMAT TSV"""
    out = subprocess.run([ch_client()], input=sql, capture_output=True, text=True, timeout=300)
    if out.returncode != 0:
        sys.exit("apilog query failed: " + out.stderr.strip()[-300:])
    rows, seen = [], set()
    for line in out.stdout.splitlines():
        f = line.split("\t")
        if len(f) < 5:
            continue
        m = PATH_RE.match(f[0])
        if not m or normalize(m.group(1)) is None:
            continue
        path = m.group(0)
        if path in seen:
            continue
        seen.add(path)
        rows.append({"path": path, "logged": f[1], "ld": f[2], "ref": urllib.parse.urlsplit(f[3]).netloc, "host": f[4]})
        if len(rows) == n:
            break
    return rows


def read_tsv(path):
    with open(path) as f:
        head = f.readline().rstrip("\n").split("\t")
        return [dict(zip(head, line.rstrip("\n").split("\t"))) for line in f if line.strip()]


def write_tsv(path, rows, cols):
    with open(path, "w") as f:
        f.write("\t".join(cols) + "\n")
        for r in rows:
            f.write("\t".join(str(r.get(c, "")).replace("\t", " ").replace("\n", " ") for c in cols) + "\n")


# --- fetching ------------------------------------------------------------------------------------------------------

def fetch(opener, url, headers, timeout, cap):
    t0 = time.monotonic()
    r = {"status": 0, "ct": "", "len": -1, "sha": "", "ms": 0, "err": "", "nosniff": "", "csp": ""}
    try:
        resp = opener.open(urllib.request.Request(url, headers=headers), timeout=timeout)
    except urllib.error.HTTPError as e:
        resp = e
    except Exception as e:  # connection refused, timeout, TLS, ...
        r["err"], r["ms"] = f"{type(e).__name__}: {e}"[:160], int((time.monotonic() - t0) * 1000)
        return r
    with resp:
        r["status"] = resp.getcode()
        h = resp.headers
        r["ct"] = (h.get("Content-Type") or "").split(";")[0].strip().lower()
        r["nosniff"] = (h.get("X-Content-Type-Options") or "").lower()
        r["csp"] = h.get("Content-Security-Policy") or ""
        sha, n = hashlib.sha256(), 0
        try:
            while n < cap:
                if time.monotonic() - t0 > timeout:
                    r["err"] = "timeout reading the body"
                    break
                chunk = resp.read(min(1 << 16, cap - n))
                if not chunk:
                    break
                sha.update(chunk)
                n += len(chunk)
        except Exception as e:
            r["err"] = f"{type(e).__name__}: {e}"[:160]
        r["len"], r["sha"] = n, sha.hexdigest()[:16]
        if n >= cap:  # compared on the first cap bytes; the length comes from the header
            r["len"], r["sha"] = int(h.get("Content-Length") or n), r["sha"] + "+cap"
    r["ms"] = int((time.monotonic() - t0) * 1000)
    return r


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def tunnel(host, jump, remote):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    p = subprocess.Popen(["ssh", *SSH_OPTS, "-o", "ExitOnForwardFailure=yes", "-N", "-L", f"127.0.0.1:{port}:{remote}",
                          "-J", jump, host], stdin=subprocess.DEVNULL)
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        if p.poll() is not None:
            sys.exit("ssh tunnel to the region host failed")
        try:
            socket.create_connection(("127.0.0.1", port), 1).close()
            return p, f"http://127.0.0.1:{port}"
        except OSError:
            time.sleep(0.3)
    p.terminate()
    sys.exit("ssh tunnel did not come up")


def catalog(host, jump, cids):
    """seapin's view of each root CID: referenced (refs), linked (links), fetch states, object status."""
    if not cids:
        return {}
    vals = ",".join(f"'{c}'" for c in sorted(cids))  # normalized base32: [a-z2-7] only
    sql = f"""BEGIN READ ONLY;
SELECT s.cid,
  EXISTS (SELECT 1 FROM refs r WHERE r.root_cid = s.cid),
  EXISTS (SELECT 1 FROM links l WHERE l.child_cid = s.cid),
  coalesce((SELECT string_agg(DISTINCT f.status || ':' || f.lane, ',') FROM fetches f WHERE f.root_cid = s.cid), ''),
  coalesce((SELECT o.status FROM objects o WHERE o.cid = s.cid), '')
FROM unnest(ARRAY[{vals}]::text[]) AS s(cid);
COMMIT;
"""
    out = subprocess.run(["ssh", *SSH_OPTS, "-J", jump, host,
                          "docker exec -i seapin-postgres psql -p 45435 -U seapin -d seapin -qAt -F '|' -v ON_ERROR_STOP=1"],
                         input=sql, capture_output=True, text=True, timeout=180)
    if out.returncode != 0:
        print("catalog query failed (continuing without it):", out.stderr.strip()[-200:], file=sys.stderr)
        return {}
    res = {}
    for line in out.stdout.splitlines():
        f = line.split("|")
        if len(f) == 5:
            res[f[0]] = {"cat_ref": f[1] == "t", "cat_link": f[2] == "t", "cat_fetch": f[3], "cat_obj": f[4]}
    return res


def refset_hits(path, sample_cids):
    """Normalized sample CIDs that the x_asset_meta extract lists (as a root, in any spelling)."""
    want = {}
    for c in sample_cids:
        for f in forms(c):
            want[f] = c
    hits = set()
    with gzip.open(path, "rt", errors="replace") as f:
        for line in f:
            p = line.split("\t", 3)
            if len(p) >= 3 and p[2] not in ("", "\\N"):
                c = want.get(p[2].split("/", 1)[0])
                if c:
                    hits.add(c)
    return hits


# --- comparison ----------------------------------------------------------------------------------------------------

def ok(s):
    return 200 <= int(s or 0) < 300


def classify(r):
    o, s = int(r["old_status"] or 0), int(r["sp_status"] or 0)
    if s == 451:
        return "takedown"
    if not ok(o):
        return "old-fail-seapin-ok" if ok(s) else "both-fail"
    if ok(s):
        if r["old_sha"] != r["sp_sha"] or r["old_len"] != r["sp_len"]:
            return "bytes-differ"
        return "identical" if r["old_ct"] == r["sp_ct"] else "type-differs"
    if s == 403:
        return "seapin-blocked"
    if s == 404:
        return "missing-referenced" if r["referenced"] else "missing-unreferenced"
    return "seapin-error"


def leak(r):
    if not ok(r["sp_status"]):
        return ""
    why = []
    if r["sp_ct"] not in ALLOWED:
        why.append("type:" + (r["sp_ct"] or "none"))
    if r["sp_nosniff"] != "nosniff":
        why.append("no-nosniff")
    if "sandbox" not in r["sp_csp"]:
        why.append("no-sandbox-csp")
    return ",".join(why)


CLASSES = ["identical", "type-differs", "bytes-differ", "seapin-blocked", "missing-referenced",
           "missing-unreferenced", "seapin-error", "takedown", "old-fail-seapin-ok", "both-fail"]
COLS = ["path", "cid", "logged", "wl", "ld", "ref", "host", "class", "leak", "referenced", "xam", "cat_ref", "cat_link",
        "cat_fetch", "cat_obj", "old_status", "old_ct", "old_len", "old_sha", "old_ms", "old_err",
        "sp_status", "sp_ct", "sp_len", "sp_sha", "sp_ms", "sp_err", "sp_nosniff", "sp_csp"]


def pct(a, b):
    return f"{100 * a / b:.1f}%" if b else "-"


def quantile(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else 0


def summarize(rows, meta):
    n = len(rows)
    by = collections.Counter(r["class"] for r in rows)
    lines = [f"seapin shadow comparison {meta['when']}: {n} distinct paths ({meta['source']})",
             f"old: {meta['old']} (cache-busted, whitelisted Referer); seapin: {meta['seapin']} "
             f"(X-Seapin-Whitelisted: {'on' if meta['whitelisted'] else 'off'})", "",
             f"{'class':<22}{'n':>6}{'share':>8}"]
    for c in CLASSES:
        if by[c]:
            lines.append(f"{c:<22}{by[c]:>6}{pct(by[c], n):>8}")
    served = [r for r in rows if ok(r["old_status"])]
    hit = [r for r in served if ok(r["sp_status"])]
    ref = [r for r in served if r["referenced"]]
    ref_hit = [r for r in ref if ok(r["sp_status"])]
    unref = [r for r in served if not r["referenced"]]
    lines += ["",
              f"old gateway served {len(served)} of {n}; seapin served {len(hit)} of those ({pct(len(hit), len(served))})",
              f"  referenced (x_asset_meta or seapin catalog): {len(ref)}, seapin served {len(ref_hit)} "
              f"({pct(len(ref_hit), len(ref))}; M4 exit: >= 99%)",
              f"  unreferenced: {len(unref)} ({pct(len(unref), len(served))} of what the old gateway served): "
              f"404 for non-whitelisted referers under D2"]
    miss = [r for r in rows if r["class"] == "missing-referenced"]
    if miss:
        st = collections.Counter(
            ("fetch " + r["cat_fetch"]) if r["cat_fetch"] else
            ("in seapin refs, no fetch" if r["cat_ref"] or r["cat_link"] else
             "not reached by the scanner yet") for r in miss)
        lines.append("  missing-referenced by seapin state: " + ", ".join(f"{k} {v}" for k, v in st.most_common()))
    blocked = collections.Counter(r["old_ct"] or "none" for r in rows if r["class"] == "seapin-blocked")
    if blocked:
        lines.append("  seapin-blocked, by the old gateway's type: " + ", ".join(f"{k} {v}" for k, v in blocked.most_common()))
    td = collections.Counter(f"{r['old_ct']} -> {r['sp_ct']}" for r in rows if r["class"] == "type-differs")
    if td:
        lines.append("  type-differs: " + ", ".join(f"{k} {v}" for k, v in td.most_common(8)))
    errs = collections.Counter(f"{r['sp_status']} {r['sp_err'][:40]}".strip() for r in rows if r["class"] == "seapin-error")
    if errs:
        lines.append("  seapin-error: " + ", ".join(f"{k} x{v}" for k, v in errs.most_common(5)))
    leaks = [r for r in rows if r["leak"]]
    lines.append(f"leaks (seapin 2xx with a type outside the allowlist, or without nosniff + sandbox CSP): {len(leaks)}")
    for r in leaks[:10]:
        lines.append(f"  LEAK {r['path']} {r['leak']}")
    lines.append(f"latency ms, 2xx: old p50 {quantile([int(r['old_ms']) for r in served], .5)} / "
                 f"p90 {quantile([int(r['old_ms']) for r in served], .9)}; seapin p50 "
                 f"{quantile([int(r['sp_ms']) for r in hit], .5)} / p90 {quantile([int(r['sp_ms']) for r in hit], .9)}")
    return "\n".join(lines) + "\n"


def env_file(key):
    v = os.environ.get(key)
    if v:
        return v
    try:
        with open(os.path.join(REPO, ".env")) as f:
            for line in f:
                if line.startswith(key + "="):
                    return line.split("=", 1)[1].strip()
    except OSError:
        pass
    return ""


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("-n", type=int, default=500, help="distinct paths to sample (default 500)")
    ap.add_argument("--hours", type=int, default=24, help="apilog window (default 24)")
    ap.add_argument("--sample", help="replay this sample.tsv instead of drawing a new one")
    ap.add_argument("--old", default="https://ipfs.algonode.xyz", help="current gateway base URL")
    ap.add_argument("--seapin", help="seapin base URL (default: ssh tunnel to the region host's 127.0.0.1:8095)")
    ap.add_argument("--ssh-host", default="root@62.83.39.60", help="region host (default net-de-nur-ipfs)")
    ap.add_argument("--ssh-jump", default="urtho@4160mgmt.hosts.algonode.net")
    ap.add_argument("--whitelisted", action="store_true", help="send X-Seapin-Whitelisted: 1 like openresty (read-through!)")
    ap.add_argument("--refset", default=os.path.expanduser("~/dev/tmp/seapin-m0/x_asset_meta_objs.tsv.gz"),
                    help="x_asset_meta extract (gzip TSV, CID in column 3); '' to skip")
    ap.add_argument("--no-catalog", action="store_true", help="skip the read-only seapin catalog query")
    ap.add_argument("--concurrency", type=int, default=MAX_CONCURRENCY, help=f"at most {MAX_CONCURRENCY}")
    ap.add_argument("--timeout", type=float, default=30)
    ap.add_argument("--cap", type=int, default=64 << 20, help="bytes hashed per body (default 64 MiB)")
    ap.add_argument("--out", default=os.environ.get("SHADOW_OUT") or os.path.expanduser("~/dev/tmp/seapin-shadow"))
    a = ap.parse_args()

    referer = env_file("SHADOW_REFERER")
    if not referer:
        sys.exit("set SHADOW_REFERER (a referer the old gateway whitelists) in the environment or .env")
    when = dt.datetime.now(dt.timezone.utc)
    out = os.path.join(a.out, when.strftime("%Y%m%dT%H%MZ"))
    os.makedirs(out, exist_ok=True)

    if a.sample:
        rows, source = read_tsv(a.sample), f"replay of {a.sample}"
    else:
        rows = draw_sample(a.n, a.hours)
        source = f"nodely.apilog, last {a.hours} h, request-weighted draw"
    write_tsv(os.path.join(out, "sample.tsv"), rows, ["path", "logged", "ld", "ref", "host"])
    for r in rows:
        r["cid"] = normalize(PATH_RE.match(r["path"]).group(1))
        r["wl"] = r["logged"] != "403"
    print(f"{len(rows)} paths ({source}); results in {out}", file=sys.stderr)

    tun = None
    seapin = a.seapin
    if not seapin:
        tun, seapin = tunnel(a.ssh_host, a.ssh_jump, "127.0.0.1:8095")
    try:
        cids = {r["cid"] for r in rows}
        cat = {} if a.no_catalog or a.seapin else catalog(a.ssh_host, a.ssh_jump, cids)
        xam = refset_hits(a.refset, cids) if a.refset and os.path.exists(a.refset) else set()

        old_open = urllib.request.build_opener()
        sp_open = urllib.request.build_opener(NoRedirect)

        def one(r):
            h = {"User-Agent": "seapin-shadow/1", "Referer": referer}
            sep = "&" if "?" in r["path"] else "?"
            res = {}
            o = fetch(old_open, a.old + r["path"] + sep + "seapin-shadow=" + secrets.token_hex(6), h, a.timeout, a.cap)
            sh = {"User-Agent": "seapin-shadow/1"}
            if a.whitelisted and r["wl"]:
                sh.update({"X-Seapin-Whitelisted": "1", "X-Seapin-Site": "seapin-shadow"})
            s = fetch(sp_open, seapin + r["path"], sh, a.timeout, a.cap)
            for k, v in o.items():
                res["old_" + k] = v
            for k, v in s.items():
                res["sp_" + k] = v
            return res

        done = 0
        with cf.ThreadPoolExecutor(max_workers=max(1, min(a.concurrency, MAX_CONCURRENCY))) as ex:
            for r, res in zip(rows, ex.map(one, rows)):
                r.update(res)
                done += 1
                if done % 50 == 0:
                    print(f"  {done}/{len(rows)}", file=sys.stderr)
    finally:
        if tun:
            tun.terminate()
            tun.wait()

    for r in rows:
        c = cat.get(r["cid"], {})
        r.update({k: c.get(k, "") for k in ("cat_ref", "cat_link", "cat_fetch", "cat_obj")})
        r["xam"] = r["cid"] in xam
        r["referenced"] = r["xam"] or c.get("cat_ref") or c.get("cat_link") or False
        r["class"], r["leak"] = classify(r), leak(r)
    write_tsv(os.path.join(out, "results.tsv"), rows, COLS)
    text = summarize(rows, {"when": when.strftime("%Y-%m-%d %H:%M UTC"), "source": source, "old": a.old,
                            "seapin": a.seapin or f"{a.ssh_host} 127.0.0.1:8095 via ssh", "whitelisted": a.whitelisted})
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(text)
    print(text, end="")


if __name__ == "__main__":
    main()
