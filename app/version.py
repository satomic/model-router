"""The build's own identity, and the project links the console offers.

Kept in its own module so the version string has exactly one home: the console reads it through
/healthz, the release check compares against it, and a release tag is matched against it. A
constant in main.py would have made every one of those an import of the whole application.
"""

VERSION = "1.1.0"

REPO_URL = "https://github.com/satomic/model-router"
ISSUES_URL = "https://github.com/satomic/model-router/issues/new"
RELEASES_URL = "https://github.com/satomic/model-router/releases/latest"

# The API the release check polls. Derived from REPO_URL by hand rather than by string surgery:
# api.github.com is a different host with a different path shape, so a rewrite would be a
# guess that silently breaks if either changes.
RELEASES_API = "https://api.github.com/repos/satomic/model-router/releases/latest"


def parse(text: str) -> tuple[int, ...] | None:
    """Turn "v1.2.3" or "1.2" into a comparable tuple, or None when it is not a version.

    Only the leading numeric dotted run is read, so "1.2.3-rc1" compares as (1, 2, 3) and a
    pre-release of a version we already run does not read as an upgrade. Returning None rather
    than a zero tuple matters: an unparseable tag must be ignored, not treated as very old.
    """
    if not text:
        return None
    body = text.strip().lstrip("vV")
    parts: list[int] = []
    for chunk in body.split("."):
        digits = ""
        for ch in chunk:
            if not ch.isdigit():
                break
            digits += ch
        if not digits:
            break
        parts.append(int(digits))
        if len(digits) != len(chunk):
            # Stop at the first suffixed segment: "3-rc1" ends the numeric run.
            break
    return tuple(parts) or None


def is_newer(candidate: str, current: str = VERSION) -> bool:
    """True when `candidate` is a strictly higher version than `current`.

    Shorter tuples are padded, so 1.2 and 1.2.0 compare equal rather than 1.2 reading as older.
    An unparseable candidate is never newer: offering an upgrade to a tag we cannot understand
    would send the user to a page that may not be a release at all.
    """
    a, b = parse(candidate), parse(current)
    if a is None or b is None:
        return False
    width = max(len(a), len(b))
    return a + (0,) * (width - len(a)) > b + (0,) * (width - len(b))
