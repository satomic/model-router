"""A five-field cron expression: `minute hour day-of-month month day-of-week`, evaluated in UTC.

Written here rather than taken from a dependency because the whole need is "when is the next
run", the syntax is the standard subset every operator already knows, and adding a package to
requirements.txt for sixty lines is not a trade worth making.

Supported per field: `*`, `N`, `A-B`, `*/S`, `A-B/S`, comma lists of those, and the usual
three-letter names for months (jan..dec) and weekdays (sun..sat). Day-of-week accepts 0-7 with
both 0 and 7 meaning Sunday. Day-of-month and day-of-week combine with OR when both are
restricted, as in Vixie cron.
"""
from datetime import datetime, timedelta, timezone

_MONTHS = {m: i + 1 for i, m in enumerate(
    ("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"))}
_DAYS = {d: i for i, d in enumerate(("sun", "mon", "tue", "wed", "thu", "fri", "sat"))}

# (min, max, name table) per field, in expression order
_FIELDS = (
    (0, 59, None),
    (0, 23, None),
    (1, 31, None),
    (1, 12, _MONTHS),
    (0, 7, _DAYS),
)

DEFAULT_SCHEDULE = "*/30 * * * *"


class CronError(ValueError):
    """The expression is not a cron schedule this module understands."""


def _atom(text: str, lo: int, hi: int, names: dict | None) -> int:
    key = text.strip().lower()
    if names and key in names:
        return names[key]
    if not key.isdigit():
        raise CronError(f"{text!r} is not a number")
    value = int(key)
    if value < lo or value > hi:
        raise CronError(f"{value} is outside {lo}-{hi}")
    return value


def _field(text: str, lo: int, hi: int, names: dict | None) -> set[int]:
    values: set[int] = set()
    for part in text.split(","):
        part = part.strip()
        if not part:
            raise CronError("empty list item")
        step = 1
        if "/" in part:
            part, step_text = part.split("/", 1)
            if not step_text.isdigit() or int(step_text) < 1:
                raise CronError(f"bad step {step_text!r}")
            step = int(step_text)
        if part == "*":
            start, end = lo, hi
        elif "-" in part:
            a, b = part.split("-", 1)
            start, end = _atom(a, lo, hi, names), _atom(b, lo, hi, names)
            if start > end:
                raise CronError(f"range {part!r} runs backwards")
        else:
            start = _atom(part, lo, hi, names)
            # `N/S` means "from N to the end, every S"; a bare N is just N
            end = hi if step > 1 else start
        values.update(range(start, end + 1, step))
    return values


def parse(expr: str) -> tuple[set[int], set[int], set[int], set[int], set[int], bool, bool]:
    """Return the five value sets plus whether day-of-month / day-of-week were restricted
    (needed for the Vixie OR rule). Raises CronError."""
    parts = (expr or "").split()
    if len(parts) != 5:
        raise CronError("expected 5 fields: minute hour day month weekday")
    sets = []
    for text, (lo, hi, names) in zip(parts, _FIELDS):
        sets.append(_field(text, lo, hi, names))
    minute, hour, dom, month, dow = sets
    if 7 in dow:
        dow.discard(7)
        dow.add(0)
    return minute, hour, dom, month, dow, parts[2] != "*", parts[4] != "*"


def validate(expr: str) -> str | None:
    """None when valid, else the reason."""
    try:
        parse(expr)
    except CronError as e:
        return str(e)
    return None


def next_after(expr: str, after: datetime) -> datetime:
    """The first matching instant strictly after `after` (UTC, second precision dropped).

    Walks forward in the coarsest unit that fails to match, so a yearly schedule is found in a
    few hundred steps rather than half a million. Bounded at five years: a schedule that never
    fires (Feb 30) is reported rather than looped on.
    """
    minute, hour, dom, month, dow, dom_set, dow_set = parse(expr)
    t = after.astimezone(timezone.utc).replace(second=0, microsecond=0) + timedelta(minutes=1)
    limit = t + timedelta(days=366 * 5)
    while t < limit:
        if t.month not in month:
            t = (t.replace(day=1, hour=0, minute=0) + timedelta(days=32)).replace(day=1)
            continue
        day_ok = _day_matches(t, dom, dow, dom_set, dow_set)
        if not day_ok:
            t = t.replace(hour=0, minute=0) + timedelta(days=1)
            continue
        if t.hour not in hour:
            t = t.replace(minute=0) + timedelta(hours=1)
            continue
        if t.minute not in minute:
            t += timedelta(minutes=1)
            continue
        return t
    raise CronError("schedule never fires")


def _day_matches(t: datetime, dom: set[int], dow: set[int], dom_set: bool, dow_set: bool) -> bool:
    # Python: Monday=0; cron: Sunday=0
    weekday = (t.weekday() + 1) % 7
    in_dom, in_dow = t.day in dom, weekday in dow
    if dom_set and dow_set:
        return in_dom or in_dow
    return in_dom and in_dow
