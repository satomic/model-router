"""Unit checks for the Copilot AI-credit pool monitor and the BYOK gate (app/aicredits.py) and
the cron parser behind its schedule (app/cronexpr.py). Everything GitHub-side is mocked; the
live derivation was verified against three real enterprises (see the module docstring).

Run: python verify/verify_ai_credits.py
"""
import time
import unittest
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import patch

import _bootstrap  # noqa: F401
from app import aicredits, cronexpr


def _cfg(**ai_credits):
    return SimpleNamespace(gh_admin_token="test-token", ai_credits=ai_credits)


def _snapshot(cfg, **entry):
    base = {
        "slug": "acme", "name": "Acme", "pool_total": 40000.0, "pool_total_source": "seats",
        "consumed": 10000.0, "metered": 0.0, "gross": 10000.0, "remaining": 30000.0,
        "state": aicredits.POOL_AVAILABLE, "seats": {"enterprise": 10}, "seat_logins": {},
        "users": None, "universal_budget_usd": None, "skus": [], "warnings": [], "error": None,
    }
    base.update(entry)
    return {
        "fetched_at": time.time(), "token_fp": aicredits.token_fp(cfg.gh_admin_token),
        "schedule": "*/30 * * * *", "per_user": False, "enterprises": {"acme": base},
    }


class CronTests(unittest.TestCase):
    def test_next_after_steps_and_ranges(self):
        t = datetime(2026, 9, 13, 10, 7, tzinfo=timezone.utc)
        self.assertEqual(cronexpr.next_after("*/30 * * * *", t), datetime(2026, 9, 13, 10, 30, tzinfo=timezone.utc))
        self.assertEqual(cronexpr.next_after("0 1 * * *", t), datetime(2026, 9, 14, 1, 0, tzinfo=timezone.utc))
        self.assertEqual(cronexpr.next_after("15 9-17/4 * * mon-fri", t), datetime(2026, 9, 14, 9, 15, tzinfo=timezone.utc))
        # Sunday as 7, and the Vixie OR rule when both day fields are restricted
        self.assertEqual(cronexpr.next_after("0 0 15 * 7", t), datetime(2026, 9, 15, 0, 0, tzinfo=timezone.utc))

    def test_validate_reports_bad_expressions(self):
        self.assertIsNone(cronexpr.validate("*/15 * * * *"))
        self.assertIn("5 fields", cronexpr.validate("* * *"))
        self.assertIn("outside", cronexpr.validate("61 * * * *"))
        self.assertIn("never fires", str(self._error("0 0 30 2 *")))

    def _error(self, expr):
        try:
            cronexpr.next_after(expr, datetime.now(timezone.utc))
        except cronexpr.CronError as e:
            return e
        return None


class PoolDerivationTests(unittest.TestCase):
    def _entry(self, **kw):
        entry = {"consumed": 0.0, "metered": 0.0, "seats": None, "warnings": []}
        entry.update(kw)
        aicredits._derive_pool(entry)
        return entry

    def test_metered_overage_means_exhausted_with_exact_size(self):
        e = self._entry(consumed=42800.0, metered=41815.9, seats={"enterprise": 10, "business": 3})
        self.assertEqual(e["state"], aicredits.POOL_EXHAUSTED)
        self.assertEqual(e["pool_total"], 42800.0)
        self.assertEqual(e["pool_total_source"], "exact")
        self.assertEqual(e["remaining"], 0.0)

    def test_seats_estimate_while_nothing_metered(self):
        e = self._entry(consumed=5000.0, seats={"enterprise": 10, "business": 2})
        self.assertEqual(e["pool_total"], 10 * 3900 + 2 * 1900)
        self.assertEqual(e["remaining"], 42800 - 5000)
        self.assertEqual(e["state"], aicredits.POOL_AVAILABLE)
        self.assertEqual(e["pool_total_source"], "seats")

    def test_no_seats_is_none_and_unknown_seats_available_once_drawn(self):
        self.assertEqual(self._entry(seats={})["state"], aicredits.POOL_NONE)
        self.assertEqual(self._entry(seats=None)["state"], aicredits.POOL_UNKNOWN)
        self.assertEqual(self._entry(seats=None, consumed=12.0)["state"], aicredits.POOL_AVAILABLE)


class GateTests(unittest.TestCase):
    def test_off_or_no_snapshot_lets_through(self):
        cfg = _cfg(gate={"enabled": False})
        with patch.object(aicredits, "load_snapshot", return_value=_snapshot(cfg)):
            self.assertIsNone(aicredits.gate(cfg, "alice"))
        cfg = _cfg(gate={"enabled": True})
        with patch.object(aicredits, "load_snapshot", return_value=None):
            self.assertIsNone(aicredits.gate(cfg, "alice"))

    def test_available_pool_gates_with_rendered_message(self):
        cfg = _cfg(gate={"enabled": True})
        with patch.object(aicredits, "load_snapshot", return_value=_snapshot(cfg)):
            verdict = aicredits.gate(cfg, "alice")
        self.assertEqual(verdict["enterprise"], "acme")
        self.assertIn("Acme", verdict["message"])
        self.assertIn("30,000 of 40,000", verdict["message"])

    def test_exhausted_stale_or_foreign_token_lets_through(self):
        cfg = _cfg(gate={"enabled": True})
        with patch.object(aicredits, "load_snapshot", return_value=_snapshot(cfg, state=aicredits.POOL_EXHAUSTED)):
            self.assertIsNone(aicredits.gate(cfg, "alice"))
        stale = _snapshot(cfg)
        stale["fetched_at"] -= 5 * 3600
        with patch.object(aicredits, "load_snapshot", return_value=stale):
            self.assertIsNone(aicredits.gate(cfg, "alice"))
        foreign = _snapshot(cfg)
        foreign["token_fp"] = "other"
        with patch.object(aicredits, "load_snapshot", return_value=foreign):
            self.assertIsNone(aicredits.gate(cfg, "alice"))

    def test_min_remaining_lifts_the_gate_early(self):
        cfg = _cfg(gate={"enabled": True}, min_remaining_credits=30000)
        with patch.object(aicredits, "load_snapshot", return_value=_snapshot(cfg)):
            self.assertIsNone(aicredits.gate(cfg, "alice"))

    def test_per_user_honours_seat_and_budget(self):
        cfg = _cfg(gate={"enabled": True, "per_user": True})
        snap = _snapshot(
            cfg,
            seat_logins={"alice": "enterprise", "bob": "business", "carol": "enterprise"},
            users={"bob": {"target_usd": 60.0, "consumed_usd": 60.0}},
            universal_budget_usd=60.0,
        )
        with patch.object(aicredits, "load_snapshot", return_value=snap):
            self.assertIsNotNone(aicredits.gate(cfg, "Alice"))      # seat, universal budget untouched
            self.assertIsNone(aicredits.gate(cfg, "bob"))           # seat, but own budget used up
            self.assertIsNotNone(aicredits.gate(cfg, "carol"))
            self.assertIsNone(aicredits.gate(cfg, "nobody"))        # no seat in this enterprise

    def test_custom_message_placeholders(self):
        cfg = _cfg(gate={"enabled": True, "message": "Use Copilot in {slug}: {remaining}/{total}"})
        with patch.object(aicredits, "load_snapshot", return_value=_snapshot(cfg)):
            self.assertEqual(aicredits.gate(cfg, "alice")["message"], "Use Copilot in acme: 30,000/40,000")


class SettingsTests(unittest.TestCase):
    def test_validate(self):
        self.assertEqual(aicredits.validate(None), [])
        self.assertEqual(aicredits.validate({"enabled": True, "schedule": "0 * * * *", "gate": {"enabled": True}}), [])
        errors = aicredits.validate({"enabled": "yes", "schedule": "x", "min_remaining_credits": -1, "gate": {"per_user": 1}})
        self.assertEqual(len(errors), 4)

    def test_status_hides_logins_from_summary_and_reports_users(self):
        cfg = _cfg(enabled=True, gate={"enabled": True, "per_user": True})
        snap = _snapshot(cfg, seat_logins={"alice": "enterprise"}, users={"alice": {"target_usd": 60.0, "consumed_usd": 10.0}})
        with patch.object(aicredits, "load_snapshot", return_value=snap):
            st = aicredits.status(cfg)
        ent = st["enterprises"][0]
        self.assertNotIn("seat_logins", ent)
        self.assertEqual(ent["seat_count"], 1)
        self.assertEqual(ent["users"][0]["headroom_usd"], 50.0)
        self.assertFalse(st["stale"])
        self.assertIsNotNone(st["next_run_at"])


if __name__ == "__main__":
    unittest.main(verbosity=1)
