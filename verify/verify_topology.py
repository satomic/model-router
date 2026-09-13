import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

import _bootstrap
from fastapi import HTTPException
from app import ghcache, main


class TopologyTests(unittest.IsolatedAsyncioTestCase):
    async def test_cached_members_are_paged_without_live_fetch(self):
        config = SimpleNamespace(gh_admin_token="test-token", key_policy={})
        data = {"token_fp": ghcache.token_fp(config.gh_admin_token), "entries": {
            "org:engineering": {"fetched_at": ghcache._now(), "logins": [f"user-{index:03}" for index in range(120)]}}}
        with patch.object(ghcache, "_load_members", return_value=data), patch.object(ghcache.ghadmin, "_rest", new_callable=AsyncMock) as fetch:
            first = await ghcache.scope_members_page(config, "organization", "engineering", 1)
            last = await ghcache.scope_members_page(config, "organization", "engineering", 3)
        self.assertEqual(len(first["users"]), 50)
        self.assertEqual(len(last["users"]), 20)
        self.assertTrue(first["has_more"])
        self.assertFalse(last["has_more"])
        self.assertEqual(first["source"], "cache")
        fetch.assert_not_awaited()

    async def test_stale_cache_fetches_only_requested_team_page(self):
        config = SimpleNamespace(gh_admin_token="test-token", key_policy={})
        with patch.object(ghcache, "_load_members", return_value={}), patch.object(ghcache.ghadmin, "_rest", new_callable=AsyncMock, return_value=(200, [{"user": {"login": "Alice"}}])) as fetch:
            result = await ghcache.scope_members_page(config, "team", "acme/42", 2)
        fetch.assert_awaited_once_with("test-token", "/enterprises/acme/teams/42/memberships?per_page=50&page=2")
        self.assertEqual(result["users"][0]["login"], "alice")
        self.assertFalse(result["has_more"])

    async def test_user_evaluates_only_selected_login_without_secrets(self):
        store = Mock()
        store.list_api_keys.return_value = [{"id": "key-one", "scope": {"kind": "all"}}]
        with patch.object(main, "_admin"), patch.object(main, "_is_admin_login", return_value=False), patch.object(main, "authstore", store), patch.object(main.keypolicy, "evaluate", new_callable=AsyncMock, return_value={"allowed": True}) as access, patch.object(main.scopepolicy, "evaluate", new_callable=AsyncMock, return_value={"allowed": False}) as scope, patch.object(main.modelpolicy, "evaluate", new_callable=AsyncMock, return_value={"models": ["cheap"]}) as models:
            result = await main.topology_user(Mock(), "Alice")
        self.assertEqual(result["login"], "alice")
        for evaluator in (access, scope, models):
            self.assertEqual(evaluator.await_args.args[1:], ("alice", False))
        store.list_api_keys.assert_called_once_with("alice", include_secret=False)
        self.assertEqual(result["model_policy"]["models"], ["cheap"])

    async def test_failed_evaluation_is_unknown_not_denied(self):
        with patch.object(main, "_admin"), patch.object(main, "_is_admin_login", return_value=False), patch.object(main.authstore, "list_api_keys", return_value=[]), patch.object(main.keypolicy, "evaluate", new_callable=AsyncMock, side_effect=TimeoutError), patch.object(main.scopepolicy, "evaluate", new_callable=AsyncMock, return_value={"allowed": False}), patch.object(main.modelpolicy, "evaluate", new_callable=AsyncMock, return_value={"models": []}):
            result = await main.topology_user(Mock(), "alice")
        self.assertIsNone(result["access"])
        self.assertEqual(result["model_policy"]["models"], [])

    async def test_non_admin_is_refused_before_data_access(self):
        with patch.object(main, "_admin", side_effect=HTTPException(403)), patch.object(main.authstore, "list_known_users") as users, patch.object(main.modelpolicy, "evaluate", new_callable=AsyncMock) as evaluate:
            with self.assertRaises(HTTPException):
                await main.topology_members(Mock(), "known", "", 1)
            with self.assertRaises(HTTPException):
                await main.topology_user(Mock(), "alice")
        users.assert_not_called()
        evaluate.assert_not_awaited()

    async def test_known_users_paged_without_eligibility(self):
        config = SimpleNamespace(admin_logins=set(), model_policy={"users": {"configured-user": "starter"}}, key_scope_policy={})
        rows = [{"login": f"user-{index:03}", "name": "User", "kind": "github"} for index in range(100)]
        with patch.object(main, "_admin"), patch.object(main, "cfg", config), patch.object(main.authstore, "list_known_users", return_value=rows), patch.object(main.keypolicy, "evaluate", new_callable=AsyncMock) as evaluate:
            result = await main.topology_members(Mock(), "known", "", 1)
        self.assertEqual(len(result["users"]), 50)
        self.assertEqual(result["users"][0]["login"], "configured-user")
        self.assertTrue(result["has_more"])
        evaluate.assert_not_awaited()


if __name__ == "__main__":
    unittest.main()