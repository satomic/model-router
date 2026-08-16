"""Verify the layered trace storage: date/user directories, one file per trace, filtered queries,
migration of legacy data.

Attribution moved from the client-reported x-user-id to the owner of the API key, so the user
directory assertions here all target the GitHub login the key belongs to; path sanitisation is
covered by unit-checking _safe() directly.
"""
import _bootstrap  # noqa: F401

import json
from pathlib import Path

import httpx

from app.config import LOG_DIR
from app.traces import _safe
from verify_auth_helper import BASE, make_client

TRACES = LOG_DIR / "traces"

client, api_key, login = make_client()
print(f"auth ready: login={login} key={api_key[:12]}…")

# 1. Storage shape: the single jsonl file is no longer used, traces live in per-directory files
#    (the .migrated marker only exists on deployments that actually migrated, so check it
#    conditionally)
assert not Path("logs/traces.jsonl").exists(), "the legacy jsonl is still there, migration did not run"
if Path("logs/traces.jsonl.migrated").exists():
    print("found logs/traces.jsonl.migrated: legacy data has been migrated")
existing = list(TRACES.glob("*/*/*.json"))
print(f"{len(existing)} individual trace files so far (logs/traces/<date>/<user>/<id>.json)")

# 2. A request carrying a session_id -> lands in the key owner's directory
r = client.post(
    "/v1/chat/completions",
    json={"messages": [{"role": "user", "content": "hi"}], "max_tokens": 20},
    headers={"x-session-id": "sess-001"},
)
r.raise_for_status()
tid = r.headers["x-trace-id"]
files = list(TRACES.glob(f"*/{_safe(login)}/{tid}.json"))
assert len(files) == 1, f"{tid}.json should exist under the {login} directory"
print("trace file:", files[0].as_posix())

# 3. No key -> 401, and no trace is produced (bare httpx, carrying no credentials at all)
anon = httpx.post(
    f"{BASE}/v1/chat/completions",
    json={"messages": [{"role": "user", "content": "hi again"}], "max_tokens": 20},
    timeout=30,
)
assert anon.status_code == 401, f"expected 401 without a key, got {anon.status_code}"
assert "x-trace-id" not in anon.headers, "a rejected request must not produce a trace"
print("no API key -> 401, no trace produced OK")

# 4. The trace carries the user_id / api_key metadata derived from the key
t = client.get(f"/v1/traces/{tid}").json()
assert t["user_id"] == login, f"user_id should be {login}, got {t['user_id']}"
assert t["session_id"] == "sess-001"
assert t["api_key_id"] and t["api_key_name"] == "verify-script"
print(f"trace records user_id={t['user_id']} session_id={t['session_id']} "
      f"api_key={t['api_key_name']}({t['api_key_id']}) OK")

# 5. Filtered queries. The listing is a page object -- {total, items, offset, limit, truncated} --
#    read straight off disk rather than out of the in-memory index.
by_user = client.get("/v1/traces", params={"user_id": login}).json()
assert by_user["items"] and all(s["user_id"] == login for s in by_user["items"])
by_sess = client.get("/v1/traces", params={"session_id": "sess-001"}).json()
assert by_sess["items"] and all(s["session_id"] == "sess-001" for s in by_sess["items"])
date = t["ts"][:10]
by_date = client.get("/v1/traces", params={"date": date}).json()
assert by_date["items"]
assert all(s["ts"].startswith(date) for s in by_date["items"]), "date filter leaked another day"
print(f"filtered queries OK: user_id={by_user['total']}, session_id={by_sess['total']}, "
      f"date={by_date['total']}")

# 6. A trace-id fragment is a substring match on the filename, which is what makes the console's
#    search box work on a partial id. Slice from the middle: trace ids are 8 hex characters, so a
#    slice starting past that would be the empty string -- which matches every trace and would make
#    this assertion pass without testing anything.
frag = tid[2:7]
assert frag and frag != tid, f"the fragment must be a real, shorter substring of {tid}"
by_frag = client.get("/v1/traces", params={"trace_id": frag}).json()
assert any(s["id"] == tid for s in by_frag["items"]), f"fragment {frag} did not find {tid}"
assert all(frag in s["id"] for s in by_frag["items"]), "the fragment filter returned a non-match"
print(f"trace_id fragment {frag!r} -> {by_frag['total']} match(es), including {tid}")

# 7. A trace beyond max_memory is still listable. This is the defect being fixed and it is
#    invisible to a smaller fixture: the in-memory index caps at 500 summaries, so the old
#    listing could not see past that no matter which filters were passed. A dated directory of
#    its own also gives the paging checks below a total that does not move under them.
FIXTURE_DAY = "2001-09-11"
FIXTURE_N = 520
assert FIXTURE_N > 500, "the fixture has to exceed TraceStore.max_memory to prove anything"
fixture_dir = TRACES / FIXTURE_DAY / _safe(login)
fixture_dir.mkdir(parents=True, exist_ok=True)
for i in range(FIXTURE_N):
    fid = f"verify-fixture-{i:04d}"
    (fixture_dir / f"{fid}.json").write_text(
        json.dumps({
            "id": fid,
            "ts": f"{FIXTURE_DAY}T00:00:{i % 60:02d}Z",
            "user_id": login,
            "strategy": "rule",
            "routing": {"model": "fixture-model", "reason": "fixture", "decision_ms": 0},
            "total_ms": 1,
            "status": "ok",
            "request": {"stream": False, "params": {}, "messages": []},
            "prompt_preview": "fixture",
        }, ensure_ascii=False),
        encoding="utf-8",
    )
oldest_id = "verify-fixture-0000"
old_page = client.get("/v1/traces", params={"date": FIXTURE_DAY, "limit": 500}).json()
assert old_page["total"] == FIXTURE_N, (old_page["total"], FIXTURE_N)
assert len(old_page["items"]) == 500, "limit is clamped to 500 per page"
found = client.get("/v1/traces", params={"trace_id": oldest_id}).json()
assert any(s["id"] == oldest_id for s in found["items"]), (
    f"{oldest_id} is beyond the {FIXTURE_N}-file day and must still be listable"
)
print(f"{FIXTURE_N} files on {FIXTURE_DAY} -> total={old_page['total']}, "
      f"the one past max_memory is still listable")

# 7b. Those fixture records carry no `turns` and no `turn_count` -- which is deliberate, because
#     that is exactly the shape of every trace written before interactions were folded into one
#     record. Both the listing and the detail have to keep reading them.
legacy = next(s for s in found["items"] if s["id"] == oldest_id)
assert legacy["turn_count"] == 1, (
    f"a pre-interaction trace should summarise as a single turn, got {legacy['turn_count']}"
)
assert legacy["interaction_id"] is None, legacy["interaction_id"]
legacy_detail = client.get(f"/v1/traces/{oldest_id}")
assert legacy_detail.status_code == 200, (legacy_detail.status_code, legacy_detail.text)
assert legacy_detail.json()["id"] == oldest_id
print("a legacy record with no turns[] still lists as turn_count=1 and still opens")

# 8. Paging over that fixture: consecutive slices are disjoint and together cover `total`.
PAGE = 200
seen: list[str] = []
for off in range(0, FIXTURE_N, PAGE):
    p = client.get("/v1/traces",
                   params={"date": FIXTURE_DAY, "offset": off, "limit": PAGE}).json()
    assert p["total"] == FIXTURE_N, (off, p["total"])
    assert p["offset"] == off and len(p["items"]) == min(PAGE, FIXTURE_N - off), (off, p["offset"])
    seen.extend(s["id"] for s in p["items"])
assert len(seen) == FIXTURE_N, (len(seen), FIXTURE_N)
assert len(set(seen)) == FIXTURE_N, "offset slices overlap"
beyond = client.get("/v1/traces",
                    params={"date": FIXTURE_DAY, "offset": FIXTURE_N, "limit": PAGE}).json()
assert beyond["items"] == [] and beyond["total"] == FIXTURE_N, beyond["total"]
print(f"paging OK: {FIXTURE_N // PAGE + 1} slices of {PAGE} cover total {FIXTURE_N}, disjoint; "
      f"offset past the end -> 0 items with total intact")

# 9. Single delete, then a second delete of the same id -> 404
victim = TRACES / FIXTURE_DAY / _safe(login) / "verify-fixture-0001.json"
assert victim.exists()
r = client.delete("/v1/traces/verify-fixture-0001")
assert r.status_code == 200, (r.status_code, r.text)
assert not victim.exists(), "the file survived the delete"
assert client.get("/v1/traces/verify-fixture-0001").status_code == 404, "the detail still resolves"
again = client.delete("/v1/traces/verify-fixture-0001")
assert again.status_code == 404, again.status_code
print("DELETE /v1/traces/<id> OK; the detail 404s and re-deleting the same id -> 404")

# 10. Batch delete returns an exact count and takes the whole day with it
remaining = client.get("/v1/traces", params={"date": FIXTURE_DAY, "limit": 500}).json()["total"]
assert remaining == FIXTURE_N - 1, (remaining, FIXTURE_N)
r = client.delete("/v1/traces", params={"date": FIXTURE_DAY, "user_id": login})
assert r.status_code == 200, (r.status_code, r.text)
assert r.json()["deleted"] == remaining, (r.json(), remaining)
after = client.get("/v1/traces", params={"date": FIXTURE_DAY, "limit": 500}).json()
assert after["total"] == 0, after["total"]
assert not (TRACES / FIXTURE_DAY).exists(), "the emptied date directory should have been pruned"
print(f"DELETE /v1/traces?date=&user_id= removed {remaining}, the date directory was pruned")

# 11. An unfiltered batch delete is refused rather than treated as a wipe-all
r = client.delete("/v1/traces")
assert r.status_code == 422, (r.status_code, r.text)
before = client.get("/v1/traces", params={"limit": 1}).json()["total"]
assert before > 0, "the refused wipe-all must have left the real traces alone"
print(f"DELETE /v1/traces with no criteria -> 422 (not a wipe-all); {before} traces intact")

# 12. Path-segment sanitisation (user_id now comes from the login, but keep the defence)
assert _safe("../../evil") == "evil", _safe("../../evil")
assert _safe("..") == "anonymous"
assert _safe("a/b\\c") == "a_b_c"
assert not Path("logs/evil").exists() and not Path("evil").exists()
print("path-traversal sanitisation OK")

print("\nALL STORAGE CHECKS PASSED")
