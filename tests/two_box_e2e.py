"""End-to-end test between TWO real machines over the tailnet (not against a mock).

Prerequisite: each machine runs a DropBridge instance with the same token, each pointing at the other via
DROPBRIDGE_PEER_URL. The test talks to both only via their tailnet addresses (like the console does) and
checks every file by sha256 on the receiving side (downloaded back through the API).

Usage: python3 tests/two_box_e2e.py <A-url> <B-url> <tokenfile> [--lan-b http://<lan-ip>:<port>] [--big-mib 1024]
       e.g. … http://100.64.0.11:18787 http://100.64.0.12:18787 /tmp/token --lan-b http://192.168.10.20:18787
Output: OK/FAIL per check plus measurements; exit 0 only if every check passed. Test files are deleted on both sides.
"""
import hashlib
import os
import sys
import time
import requests

args = sys.argv[1:]
lan_b = None
big = 1024
if "--lan-b" in args:
    k = args.index("--lan-b"); lan_b = args[k + 1]; del args[k:k + 2]
if "--big-mib" in args:
    k = args.index("--big-mib"); big = int(args[k + 1]); del args[k:k + 2]
A, B, TOKF = args
TOKEN = open(TOKF).read().strip()
results, created = [], {A: set(), B: set()}
TAG = time.strftime("e2e-%H%M%S")


def check(name, ok, detail=""):
    results.append(ok)
    print(("OK   " if ok else "FAIL ") + name + (f"  [{detail}]" if detail else ""), flush=True)


def sha(b):
    return hashlib.sha256(b).hexdigest()


def send(src, name, data, target="peer"):
    """Send a file the way the browser does (multipart field `files`, filename = relative path)."""
    t0 = time.time()
    r = requests.post(f"{src}/api/send?target={target}", files=[("files", (name, data))], timeout=3600)
    return r, time.time() - t0


def received(at):
    r = requests.get(f"{at}/api/received", timeout=30)
    return {it["name"]: it for it in r.json().get("items", [])} if r.ok else {}


def download(at, name):
    r = requests.get(f"{at}/api/download", params={"name": name}, timeout=3600)
    return r.content if r.ok else None


# 1 — reachability + guard
for n, u in (("A", A), ("B", B)):
    c = requests.get(f"{u}/api/config", timeout=15)
    m = requests.get(f"{u}/api/mesh", timeout=15)
    check(f"{n}: /api/config + /api/mesh over the tailnet", c.ok and m.ok, f"{c.status_code}/{m.status_code} node={c.json().get('node') if c.ok else '-'}")
if lan_b:
    try:
        r = requests.get(f"{lan_b}/api/mesh", timeout=10)
        check("B: /api/mesh via the LAN address is refused (403)", r.status_code == 403, f"HTTP {r.status_code}")
        r = requests.post(f"{lan_b}/api/send?target=peer", files=[("files", ("lan.txt", b"x"))], timeout=10)
        check("B: /api/send via the LAN address is refused (403)", r.status_code == 403, f"HTTP {r.status_code}")
    except requests.RequestException as e:
        check("B: LAN address reachable for the guard check", False, str(e)[:80])

# 2 — mesh: peer online, measured latency, presence
for n, u in (("A", A), ("B", B)):
    d = requests.get(f"{u}/api/mesh", timeout=15).json()
    peers = d.get("peers", [])
    p = peers[0] if peers else {}
    check(f"{n}: peer online in the mesh with measured latency", bool(p.get("online")) and (p.get("latencyMs") or 0) > 0,
           f"peer={p.get('id')} online={p.get('online')} latency={p.get('latencyMs')} ms")
    check(f"{n}: own node telemetry (kernel, Tailscale version)", bool(d["self"].get("kernel")) and bool(d["self"].get("tailscale")),
           f"kernel={d['self'].get('kernel')} ts={d['self'].get('tailscale')} users={len(d['self'].get('users') or [])}")

# 3/4 — small file both ways, sha256 on the receiving side
for src, dst, rn in ((A, B, "A→B"), (B, A, "B→A")):
    name = f"{TAG}-{rn.replace('→', '-to-')}.bin"
    data = os.urandom(1024 * 1024 + 17)
    r, dt = send(src, name, data)
    res = (r.json().get("results") or [{}]) if r.headers.get("content-type", "").startswith("application/json") else [{}]
    check(f"{rn}: send reports success", r.ok and all(x.get("ok") for x in res), f"HTTP {r.status_code} {dt:.2f} s")
    created[dst].add(name)
    got = download(dst, name)
    check(f"{rn}: file arrives bit-exact (sha256)", got is not None and sha(got) == sha(data), f"{len(data)} B")

# 5 — big file A→B (streaming, no size cap), throughput
data = os.urandom(big * 1024 * 1024)
name = f"{TAG}-gross-{big}MiB.bin"
r, dt = send(A, name, data)
check(f"A→B: {big} MiB send", r.ok and all(x.get("ok") for x in (r.json().get("results") or [{}])),
       f"HTTP {r.status_code} in {dt:.1f} s = {big / dt:.1f} MiB/s")
created[B].add(name)
item = received(B).get(name, {})
check(f"A→B: {big} MiB arrived complete (size)", item.get("bytes") == len(data), f"{item.get('bytes')} of {len(data)} B")
got = download(B, name)
check(f"A→B: {big} MiB bit-exact (sha256, downloaded back)", got is not None and sha(got) == sha(data))
del data, got

# 6 — folder structure
parts = {f"{TAG}-folder/2026/summer/a.txt": b"alpha", f"{TAG}-folder/2026/b.txt": b"beta", f"{TAG}-folder/c.txt": b"gamma"}
for rel, content in parts.items():
    send(A, rel, content)
    created[B].add(rel)
rb = received(B)
check("folder: relative paths kept", all(rel in rb for rel in parts), ", ".join(k.split(TAG)[1] for k in parts if k not in rb) or "3/3 paths")
check("folder: contents match", all(download(B, rel) == c for rel, c in parts.items()))

# 7 — name conflict → renamed, nothing overwritten
name = f"{TAG}-conflict.txt"
send(A, name, b"first")
send(A, name, b"second")
rb = received(B)
new = [k for k in rb if k.startswith(f"{TAG}-conflict") and k != name]
created[B].update([name] + new)
check("conflict: second file renamed, first kept", download(B, name) == b"first" and len(new) == 1 and download(B, new[0]) == b"second",
       f"new={new}")

# 8 — ingest auth: wrong / no token
for how, h in (("wrong token", {"Authorization": "Bearer wrong-" + TOKEN[:4]}), ("no token", {})):
    r = requests.post(f"{B}/api/ingest", data=b"evil", headers={"X-Filename": f"{TAG}-evil.txt", **h}, timeout=15)
    check(f"ingest with {how} is refused", r.status_code == 401, f"HTTP {r.status_code}")
check("refused file did not arrive", f"{TAG}-evil.txt" not in received(B))

# 9 — path traversal in the filename
evil = f"../../../../tmp/{TAG}-escape.txt"
r, _ = send(A, evil, b"path")
rb = received(B)
hits = [k for k in rb if f"{TAG}-escape" in k]
created[B].update(hits)
check("path traversal: defused inside the received dir (or refused)", all(".." not in k and not k.startswith("/") for k in hits),
       f"HTTP {r.status_code}, stored as {hits}")

# 10 — share link (without funnel: tailnet only), revoke
name = f"{TAG}-A-to-B.bin"
r = requests.post(f"{B}/api/share", params={"name": name}, timeout=15)
j = r.json() if r.ok else {}
tok = j.get("token") or (j.get("url", "").rsplit("/s/", 1)[-1] if "/s/" in j.get("url", "") else None)
check("create share", r.ok and bool(tok), f"public={j.get('public')} (no funnel configured)")
if tok:
    s = requests.get(f"{B}/s/{tok}", timeout=30)
    check("share link serves the file", s.ok and sha(s.content) == sha(download(B, name) or b""), f"HTTP {s.status_code}")
    u = requests.post(f"{B}/api/unshare", params={"token": tok}, timeout=15)
    s2 = requests.get(f"{B}/s/{tok}", timeout=15)
    check("revoked link is dead", u.ok and s2.status_code == 404, f"unshare {u.status_code}, then HTTP {s2.status_code}")

# 11 — delete (cleanup) + cross-check
for at in (A, B):
    for name in sorted(created[at]):
        requests.post(f"{at}/api/delete", params={"name": name}, timeout=30)
left = [k for at in (A, B) for k in received(at) if TAG in k]
check("cleanup: all test files deleted on both sides", not left, f"left: {left}" if left else "")

print(f"\n{len(results)} checks, {results.count(False)} failed")
sys.exit(0 if all(results) else 1)
