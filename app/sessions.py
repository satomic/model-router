"""Session stickiness: an in-memory TTL + LRU store of session_id -> chosen model."""
import time
from collections import OrderedDict
from threading import Lock


class SessionStore:
    def __init__(self, ttl_seconds: int, max_sessions: int):
        self.ttl = ttl_seconds
        self.max_sessions = max_sessions
        self._data: OrderedDict[str, tuple[str, float]] = OrderedDict()
        self._lock = Lock()

    def get(self, session_id: str) -> str | None:
        with self._lock:
            item = self._data.get(session_id)
            if item is None:
                return None
            model, expires = item
            if time.monotonic() > expires:
                del self._data[session_id]
                return None
            self._data.move_to_end(session_id)
            return model

    def set(self, session_id: str, model: str) -> None:
        with self._lock:
            self._data[session_id] = (model, time.monotonic() + self.ttl)
            self._data.move_to_end(session_id)
            while len(self._data) > self.max_sessions:
                self._data.popitem(last=False)
