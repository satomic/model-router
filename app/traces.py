"""Storage for full call traces.

Layout: logs/traces/<YYYY-MM-DD>/<user_id>/<trace_id>.json -- one file per *user
interaction*, which makes querying by date, user or trace id straightforward. The most
recent N summaries are indexed in memory so listings are fast.

One file is not one HTTP request. An agentic client (GitHub Copilot, for one) answers a
single user question with a whole loop of requests: it calls the model, runs the tool the
model asked for, appends the result and calls again, repeating until the model stops asking
for tools. Every one of those requests carries the same interaction id, so add() folds them
into one file as successive `turns` -- the record of what the user did stays one record, and
the tool calls that made it up are all inside it.
"""
import json
import re
import threading
from collections import OrderedDict
from pathlib import Path

_SAFE_SEGMENT = re.compile(r"[^\w.\-]")

# How many date *directories* query() will look at when no explicit date is given. This is a
# directory count, not a calendar window: over an idle deployment 60 directories reach back
# much further than 60 days, and over a busy one they are simply the 60 most recent days.
_MAX_QUERY_DATES = 60

# A ceiling on how many turns one interaction record keeps. An agent stuck in a tool loop
# would otherwise grow a single file without bound; past this the turns are counted but no
# longer stored, and `turns_truncated` says so rather than the record quietly lying.
_MAX_TURNS = 200


def _safe(segment: str) -> str:
    """Sanitise a path segment against directory traversal and illegal characters;
    leading/trailing dots are stripped so '..' cannot survive."""
    s = _SAFE_SEGMENT.sub("_", segment)[:64].strip("._")
    return s or "anonymous"


def _summarize(trace: dict) -> dict:
    return {
        "id": trace["id"],
        "ts": trace["ts"],
        "user_id": trace.get("user_id"),
        "session_id": trace.get("session_id"),
        # The client-supplied id that ties the turns of one user interaction together, and how
        # many turns this record ended up holding. A one-turn trace is the ordinary case; more
        # than one means an agentic tool loop.
        "interaction_id": trace.get("interaction_id"),
        "turn_count": trace.get("turn_count") or 1,
        "strategy": trace.get("strategy"),
        "model": trace.get("routing", {}).get("model"),
        "reason": trace.get("routing", {}).get("reason"),
        "decision_ms": trace.get("routing", {}).get("decision_ms"),
        "total_ms": trace.get("total_ms"),
        "status": trace.get("status"),
        "stream": trace.get("request", {}).get("stream"),
        "prompt_preview": trace.get("prompt_preview"),
    }


def _sum_usage(turns: list[dict]) -> dict | None:
    """Add up the token usage of every turn.

    The replayed conversation is deliberately counted once per turn: each request really did
    send the whole chain upstream and really was billed for it, so the interaction's cost is
    the sum, not the last turn's figure. Returns None when no turn reported usage at all --
    a zeroed usage block would read as "this was free".
    """
    fields = ("prompt_tokens", "completion_tokens", "total_tokens")
    totals = {f: 0 for f in fields}
    seen = False
    for turn in turns:
        usage = ((turn.get("response") or {}).get("usage")) or {}
        if not usage:
            continue
        seen = True
        for f in fields:
            totals[f] += usage.get(f) or 0
    return totals if seen else None


def _merge_turn(base: dict, incoming: dict) -> dict:
    """Fold a follow-up request into the interaction record it belongs to.

    What is kept from `base`: the id, the opening timestamp, the prompt preview and -- most
    importantly -- `routing`. The model was chosen once for the whole interaction, so a
    second decision block would be a decision that never happened.

    What is taken from `incoming`: the accumulated message chain, the latest response and
    status, and the backend that served it. `request.messages` therefore always holds the
    complete conversation including every tool call and tool result, which is what makes the
    single record a full chain rather than a fragment.
    """
    doc = base
    prior = doc.get("turns") or []
    prev_messages = (doc.get("request") or {}).get("messages") or []
    new_turns = incoming.get("turns") or []

    for turn in new_turns:
        turn["index"] = len(prior) + 1
        # An agentic client appends to the chain and replays it, so a turn's request is the
        # first `message_count` messages of the final chain -- no need to store a second copy
        # of a conversation that is already recorded in full at the top level. When the client
        # rewrote history instead of appending, that reconstruction would be wrong, so those
        # turns carry their own messages and say why.
        count = turn.get("message_count") or 0
        # `>=`, not `>`: the opening turn's chain *is* the one stored at the top level, so it
        # is reconstructible too and a second copy of it would double the record for nothing.
        appended = count >= len(prev_messages) and (
            not prev_messages
            or _same_message(prev_messages[-1], turn.get("messages") or [], len(prev_messages) - 1)
        )
        if appended:
            turn.pop("messages", None)
        elif turn.get("messages") is not None:
            turn["rewritten"] = True
            # This turn's chain is not an extension of the previous one, so the top level is
            # about to stop being a superset of what came before -- and the earlier turns were
            # stored without their own copies precisely because it was. Give the last of them
            # its chain back before that stops being true.
            if prior and prior[-1].get("messages") is None:
                prior[-1]["messages"] = prev_messages
                prior[-1]["superseded"] = True
        # The parameters are repeated verbatim on every turn of a Copilot loop, and `tools`
        # alone is far bigger than the rest of the record -- keep them only when they differ
        # from the ones already stored at the top level.
        if turn.get("params") == (doc.get("request") or {}).get("params"):
            turn.pop("params", None)
        prior.append(turn)

    truncated = doc.get("turns_truncated") or 0
    if len(prior) > _MAX_TURNS:
        truncated += len(prior) - _MAX_TURNS
        prior = prior[-_MAX_TURNS:]

    doc["turns"] = prior
    # Counts every turn that happened, including any dropped, so the number always matches
    # what the client did rather than what survived the cap.
    doc["turn_count"] = (doc.get("turn_count") or 0) + len(new_turns)
    if truncated:
        doc["turns_truncated"] = truncated

    doc["request"] = incoming.get("request") or doc.get("request")
    doc["backend"] = incoming.get("backend") or doc.get("backend")
    doc["status"] = incoming.get("status")
    if incoming.get("error"):
        doc["error"] = incoming["error"]
    else:
        doc.pop("error", None)

    # The interaction's token cost, at the top level rather than only inside `response`: an
    # interaction whose last turn failed still spent everything the earlier turns spent, and
    # hanging the total off a response that is None would report it as free.
    doc["usage"] = _sum_usage(doc["turns"])
    response = incoming.get("response")
    if response is not None:
        # Content and finish_reason come from the closing turn -- that is the answer the user
        # actually read -- while usage is the whole interaction's.
        doc["response"] = {**response, "usage": doc["usage"]}
    else:
        doc["response"] = None

    total = sum(t.get("total_ms") or 0 for t in doc["turns"])
    doc["total_ms"] = round(total, 1)
    decision_ms = (doc.get("routing") or {}).get("decision_ms") or 0
    # Same relationship as a single-turn trace (total = decision + backend), just summed, so
    # the console's latency breakdown keeps adding up.
    doc.setdefault("backend", {})["latency_ms"] = round(total - decision_ms, 1)
    return doc


def _same_message(previous: dict, messages: list, index: int) -> bool:
    """Whether `messages[index]` is still the message the previous turn ended on.

    A length comparison alone would call a trimmed chain an append; comparing the one
    boundary message catches that in constant time, which a full prefix compare on a long
    conversation would not be worth.
    """
    if index < 0 or index >= len(messages):
        return False
    candidate = messages[index]
    if not isinstance(candidate, dict) or not isinstance(previous, dict):
        return candidate == previous
    return (candidate.get("role") == previous.get("role")
            and candidate.get("content") == previous.get("content"))


class TraceStore:
    def __init__(self, root: Path, max_memory: int = 500):
        self.root = root
        self.max_memory = max_memory
        self._index: OrderedDict[str, dict] = OrderedDict()  # id -> {summary, path}
        # interaction_id -> trace id, so a follow-up turn finds its record without a disk
        # search. Only ever a cache: _interaction_path falls back to the directory when an
        # entry is missing, which is what makes this survive a restart or an eviction.
        self._interactions: dict[str, str] = {}
        self._lock = threading.Lock()
        root.mkdir(parents=True, exist_ok=True)
        self._migrate_legacy_jsonl()
        self._load_recent()

    def _trace_path(self, trace: dict) -> Path:
        date = trace["ts"][:10]
        user = _safe(trace.get("user_id") or "anonymous")
        return self.root / date / user / f"{_safe(trace['id'])}.json"

    def _migrate_legacy_jsonl(self) -> None:
        """Split the legacy single-file traces.jsonl out into the new directory layout."""
        legacy = self.root.parent / "traces.jsonl"
        if not legacy.exists():
            return
        migrated = 0
        try:
            with open(legacy, encoding="utf-8") as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    trace = json.loads(line)
                    path = self._trace_path(trace)
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(
                        json.dumps(trace, ensure_ascii=False, indent=2), encoding="utf-8"
                    )
                    migrated += 1
            legacy.rename(legacy.with_suffix(".jsonl.migrated"))
        except Exception:  # noqa: BLE001 a failed migration must not block startup
            pass

    def _load_recent(self) -> None:
        """Walk the date directories newest-first, loading recent trace files into the
        in-memory index."""
        files: list[Path] = []
        for date_dir in sorted(self.root.iterdir(), reverse=True):
            if not date_dir.is_dir():
                continue
            files.extend(date_dir.glob("*/*.json"))
            if len(files) >= self.max_memory:
                break
        files.sort(key=lambda p: p.stat().st_mtime)
        for path in files[-self.max_memory:]:
            trace = self._read(path)
            if trace is None or "id" not in trace:  # skip corrupt files
                continue
            self._index[trace["id"]] = {"summary": _summarize(trace), "path": path}
            if trace.get("interaction_id"):
                self._interactions[trace["interaction_id"]] = trace["id"]

    def add(self, trace: dict) -> None:
        """Persist a turn, folding it into the interaction record it belongs to.

        The whole read-merge-write runs under the store lock: the turns of one interaction
        are sequential from the client's point of view, but nothing stops two of them
        overlapping at the server, and a lost update here would silently drop a tool call
        from the chain.
        """
        with self._lock:
            existing_path = self._interaction_path(trace)
            if existing_path is not None:
                base = self._read(existing_path)
            else:
                base = None
            if base is None:
                doc = _merge_turn(self._new_record(trace), trace)
                path = self._trace_path(doc)
            else:
                doc = _merge_turn(base, trace)
                path = existing_path
                # The caller minted an id for this turn; the record keeps the one it opened
                # with, so `x-trace-id` on the response points at the interaction.
                trace["id"] = doc["id"]

            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(doc, ensure_ascii=False, indent=2), encoding="utf-8")
            self._index[doc["id"]] = {"summary": _summarize(doc), "path": path}
            self._index.move_to_end(doc["id"])
            interaction = doc.get("interaction_id")
            if interaction:
                self._interactions[interaction] = doc["id"]
            while len(self._index) > self.max_memory:
                evicted, _ = self._index.popitem(last=False)
                # Drop the interaction pointer with the summary it pointed at, or a long-lived
                # interaction id would keep resolving to an id no longer in the index.
                for iid, tid in list(self._interactions.items()):
                    if tid == evicted:
                        del self._interactions[iid]

    def resolve_interaction(self, trace: dict) -> str:
        """Point `trace['id']` at the record this turn will be folded into, and return it.

        Called before the upstream call so the `x-trace-id` response header names the
        interaction rather than a per-request id that would resolve to nothing. add() repeats
        the lookup under the lock, so this is an optimisation for the header only and a stale
        answer here cannot split a record.
        """
        with self._lock:
            path = self._interaction_path(trace)
            doc = self._read(path) if path is not None else None
        if doc and doc.get("id"):
            trace["id"] = doc["id"]
        return trace["id"]

    @staticmethod
    def _new_record(trace: dict) -> dict:
        """The interaction record a first turn opens: everything except the per-turn fields,
        which _merge_turn adds."""
        doc = {k: v for k, v in trace.items() if k not in ("turns", "response", "error")}
        doc["turns"] = []
        doc["turn_count"] = 0
        return doc

    def _interaction_path(self, trace: dict) -> Path | None:
        """The file already holding this interaction, or None if this turn opens it.

        Called with the lock held. The in-memory pointer answers the common case; the glob is
        the fallback for an interaction whose summary has been evicted or that predates this
        worker, scoped to the one date/user directory the record can possibly be in.
        """
        interaction = trace.get("interaction_id")
        if not interaction:
            return None
        known = self._interactions.get(interaction)
        if known:
            entry = self._index.get(known)
            if entry and entry["path"].exists():
                return entry["path"]
        directory = self._trace_path(trace).parent
        if not directory.is_dir():
            return None
        for path in directory.glob("*.json"):
            doc = self._read(path)
            if doc and doc.get("interaction_id") == interaction:
                self._interactions[interaction] = doc.get("id") or path.stem
                return path
        return None

    @staticmethod
    def _read(path: Path) -> dict | None:
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except Exception:  # noqa: BLE001 a corrupt file must not fail the request being traced
            return None

    def list(
        self,
        limit: int = 50,
        date: str | None = None,
        user_id: str | None = None,
        session_id: str | None = None,
    ) -> list[dict]:
        """Return trace summaries newest-first, optionally filtered by date, user or
        session."""
        with self._lock:
            summaries = [e["summary"] for e in self._index.values()]
        result = []
        for s in reversed(summaries):
            if date and not (s["ts"] or "").startswith(date):
                continue
            if user_id and s.get("user_id") != user_id:
                continue
            if session_id and s.get("session_id") != session_id:
                continue
            result.append(s)
            if len(result) >= limit:
                break
        return result

    def query(
        self,
        date: str | None = None,
        user_id: str | None = None,
        trace_id: str | None = None,
        session_id: str | None = None,
        offset: int = 0,
        limit: int = 50,
        max_dates: int = _MAX_QUERY_DATES,
    ) -> dict:
        """Return {total, items, offset, limit, truncated} read straight off disk.

        This is what the console lists, rather than the in-memory index: the index only
        holds the most recent max_memory summaries, so anything older was invisible to a
        listing no matter which filters were passed.

        Ordering is by (date directory, file mtime) rather than by the `ts` inside each
        file. mtime is written when the trace is persisted -- at the end of the very
        request its ts opens -- so the two orders agree, and mtime comes from stat(),
        which means a page costs `limit` file reads instead of one read per trace on disk.

        `date` and `user_id` are directory names, so filtering on them narrows the walk
        instead of scanning everything. `trace_id` matches as a substring of the filename,
        which is what makes the console's search box work on a partial id. `session_id`
        lives *inside* the file, so it is the one filter that costs a read per candidate --
        pair it with a date or a user to keep that bounded.
        """
        if not self.root.exists():
            return {"total": 0, "items": [], "offset": offset, "limit": limit, "truncated": False}

        truncated = False
        if date:
            date_dirs = [self.root / date]
        else:
            all_dirs = sorted((d for d in self.root.iterdir() if d.is_dir()), reverse=True)
            date_dirs = all_dirs[: max(max_dates, 1)]
            # Reported so the console can say the count is a floor rather than a total: a
            # silently shortened total reads as "that is all there is".
            truncated = len(all_dirs) > len(date_dirs)

        pattern = f"*{_safe(trace_id)}*.json" if trace_id else "*.json"
        # (date directory name, mtime, path) -- the name sorts lexicographically, which for
        # YYYY-MM-DD is also chronological.
        candidates: list[tuple[str, float, Path]] = []
        for date_dir in date_dirs:
            if not date_dir.is_dir():
                continue
            user_dirs = (
                [date_dir / _safe(user_id)] if user_id else
                [d for d in date_dir.iterdir() if d.is_dir()]
            )
            for user_dir in user_dirs:
                if not user_dir.is_dir():
                    continue
                for path in user_dir.glob(pattern):
                    try:
                        candidates.append((date_dir.name, path.stat().st_mtime, path))
                    except OSError:  # deleted between the glob and the stat
                        continue

        candidates.sort(key=lambda c: (c[0], c[1]), reverse=True)
        offset = max(offset, 0)

        if session_id:
            # Not a path segment, so every candidate has to be opened. Done before paging so
            # `total` still counts matches rather than files looked at.
            matched: list[dict] = []
            for _, _, path in candidates:
                trace = self._read(path)
                if trace is None or "id" not in trace:  # skip corrupt files
                    continue
                if trace.get("session_id") == session_id:
                    matched.append(_summarize(trace))
            return {
                "total": len(matched),
                "items": matched[offset: offset + max(limit, 0)],
                "offset": offset,
                "limit": limit,
                "truncated": truncated,
            }

        total = len(candidates)
        items: list[dict] = []
        for _, _, path in candidates[offset: offset + max(limit, 0)]:
            trace = self._read(path)
            if trace is None or "id" not in trace:  # skip corrupt files
                continue
            items.append(_summarize(trace))
        return {
            "total": total,
            "items": items,
            "offset": offset,
            "limit": limit,
            "truncated": truncated,
        }

    def delete(self, trace_id: str) -> bool:
        """Delete one trace by id. Returns False when it does not exist."""
        safe = _safe(trace_id)
        with self._lock:
            entry = self._index.pop(trace_id, None)
            self._forget_interactions(trace_id)
        paths = [entry["path"]] if entry else list(self.root.glob(f"*/*/{safe}.json"))
        removed = False
        for path in paths:
            try:
                path.unlink()
                removed = True
            except FileNotFoundError:
                continue
            self._prune_dirs(path.parent)
        return removed

    def delete_many(self, date: str | None = None, user_id: str | None = None) -> int:
        """Delete every trace matching date and/or user, returning how many went.

        At least one criterion is required: an argument-less call would be a wipe-all,
        which no caller is allowed to reach for by accident.
        """
        if not date and not user_id:
            raise ValueError("delete_many requires date and/or user_id")
        date_dirs = (
            [self.root / date] if date else
            [d for d in self.root.iterdir() if d.is_dir()] if self.root.exists() else []
        )
        deleted = 0
        for date_dir in date_dirs:
            if not date_dir.is_dir():
                continue
            user_dirs = (
                [date_dir / _safe(user_id)] if user_id else
                [d for d in date_dir.iterdir() if d.is_dir()]
            )
            for user_dir in user_dirs:
                if not user_dir.is_dir():
                    continue
                for path in list(user_dir.glob("*.json")):
                    try:
                        path.unlink()
                    except OSError:
                        continue
                    deleted += 1
                    # Drop the in-memory summary too: a stale entry would keep serving
                    # get() from a path that no longer exists, and a stale interaction
                    # pointer would make the next turn try to append to a deleted file.
                    with self._lock:
                        self._index.pop(path.stem, None)
                        self._forget_interactions(path.stem)
                self._prune_dirs(user_dir)
        return deleted

    def _forget_interactions(self, trace_id: str) -> None:
        """Drop every interaction pointer aimed at a trace id. Called with the lock held."""
        for iid, tid in list(self._interactions.items()):
            if tid == trace_id:
                del self._interactions[iid]

    def _prune_dirs(self, user_dir: Path) -> None:
        """Remove an emptied <user> directory and, if it was the last one, its <date>
        parent -- so a deleted day stops appearing as an empty bucket in query()."""
        for directory in (user_dir, user_dir.parent):
            try:
                if directory != self.root and directory.is_dir() and not any(directory.iterdir()):
                    directory.rmdir()
            except OSError:
                return

    def scan(
        self, days: int = 7, user_login: str | None = None
    ) -> "list[dict]":  # Annotation quoted: the same-named list method shadows builtin list
        """Read trace summaries for the last `days` days straight off disk, for usage
        aggregation.

        The in-memory index only holds the most recent max_memory entries, which is not
        enough for per-day statistics, hence reading the files here.
        When user_login is not None, only that user's directory is scanned.
        """
        date_dirs = sorted(
            (d for d in self.root.iterdir() if d.is_dir()), reverse=True
        )[: max(days, 1)]
        summaries: list[dict] = []
        for date_dir in date_dirs:
            user_dirs = (
                [date_dir / _safe(user_login)] if user_login else
                [d for d in date_dir.iterdir() if d.is_dir()]
            )
            for user_dir in user_dirs:
                if not user_dir.is_dir():
                    continue
                for path in user_dir.glob("*.json"):
                    trace = self._read(path)
                    if trace is None or "id" not in trace:  # skip corrupt files
                        continue
                    summary = _summarize(trace)
                    # The top-level total covers every turn of the interaction and survives a
                    # failed final turn; the response block is the pre-turns fallback.
                    usage = (
                        trace.get("usage")
                        or (trace.get("response") or {}).get("usage")
                        or {}
                    )
                    summary["usage"] = {
                        "prompt_tokens": usage.get("prompt_tokens") or 0,
                        "completion_tokens": usage.get("completion_tokens") or 0,
                        "total_tokens": usage.get("total_tokens") or 0,
                    }
                    summaries.append(summary)
        summaries.sort(key=lambda s: s.get("ts") or "", reverse=True)
        return summaries

    def get(self, trace_id: str) -> dict | None:
        with self._lock:
            entry = self._index.get(trace_id)
        path = entry["path"] if entry else None
        if path is None:  # Not in the in-memory index, so search the whole tree
            matches = list(self.root.glob(f"*/*/{_safe(trace_id)}.json"))
            path = matches[0] if matches else None
        if path is None or not path.exists():
            return None
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except Exception:  # noqa: BLE001
            return None
