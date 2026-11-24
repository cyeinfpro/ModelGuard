import importlib.util
import json
from pathlib import Path
import unittest
from unittest.mock import patch
spec = importlib.util.spec_from_file_location("reconcile_bridge", Path(__file__).resolve().parents[1] / "scripts/reconcile-api-bridge.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

class ReconcileTests(unittest.TestCase):
    def setUp(self):
        self.compose = ["docker", "compose", "-f", "/test/compose.yml"]
        self.host = {"id": "host", "pid": 1, "namespace": "net:[1]"}
        self.old = {"id": "bridge", "pid": 2, "namespace": "net:[2]"}
        self.new = {"id": "newbridge", "pid": 3, "namespace": "net:[1]"}
        self.config = json.dumps({"services": {
            "modelguard-api-bridge": {"network_mode": "service:sub2api"},
        }})
        self.recreate = self.compose + [
            "up", "-d", "--no-deps", "--force-recreate", "--pull", "never",
            "--no-build", "modelguard-api-bridge",
        ]

    def test_matching_namespaces_are_noop_even_apply(self):
        with patch.object(module, "snapshot", return_value=[self.host,self.new]), patch.object(module,"run") as run:
            module.reconcile(self.compose, True)
            run.assert_not_called()

    def test_default_is_check_only(self):
        with patch.object(module, "snapshot", return_value=[self.host,self.old]), patch.object(module,"run") as run:
            with self.assertRaises(module.ReconcileError):
                module.reconcile(self.compose)
            run.assert_not_called()

    def test_apply_recreates_only_bridge_and_verifies(self):
        with patch.object(module, "snapshot", side_effect=[
            [self.host, self.old], [self.host, self.old], [self.host, self.new],
        ]), patch.object(module, "run", return_value=self.config) as run:
            module.reconcile(self.compose, True)
            self.assertEqual(run.call_args_list[0].args[0], self.compose + ["config", "--format", "json"])
            self.assertEqual(run.call_args_list[1].args[0], self.recreate)

    def test_concurrent_host_change_aborts_before_write(self):
        with patch.object(module, "snapshot", side_effect=[
            [self.host, self.old], [dict(self.host, pid=9), self.old],
        ]), patch.object(module, "run", return_value=self.config) as run:
            with self.assertRaises(module.ReconcileError):
                module.reconcile(self.compose, True)
            run.assert_called_once_with(self.compose + ["config", "--format", "json"])

    def test_wrong_compose_network_aborts_before_write(self):
        with patch.object(module, "snapshot", return_value=[self.host, self.old]), \
             patch.object(module, "run", return_value='{"services":{}}') as run:
            with self.assertRaises(module.ReconcileError):
                module.reconcile(self.compose, True)
            run.assert_called_once_with(self.compose + ["config", "--format", "json"])

    def test_uncertain_command_outcome_is_not_retried(self):
        with patch.object(module, "snapshot", return_value=[self.host, self.old]), \
             patch.object(module, "run", side_effect=[self.config, module.ReconcileError("failed")]) as run:
            with self.assertRaises(module.ReconcileError):
                module.reconcile(self.compose, True)
            self.assertEqual(run.call_count, 2)

    def test_failed_postcheck_is_reported(self):
        with patch.object(module, "snapshot", return_value=[self.host, self.old]), \
             patch.object(module, "run", return_value=self.config) as run:
            with self.assertRaises(module.ReconcileError):
                module.reconcile(self.compose, True)
            self.assertEqual(run.call_count, 2)

    def test_snapshot_detects_restart_and_replacement(self):
        host = json.dumps(dict(self.host, running=True, network="sub2api-network"))
        for target in ("host", "replaced-host"):
            bridge = json.dumps(dict(self.old, running=True, network="container:" + target))
            with self.subTest(target=target), patch.object(module, "run", side_effect=[
                "host", host, "bridge", bridge,
            ]), patch.object(module.os, "readlink", side_effect=["net:[1]", "net:[2]"]):
                host_state, bridge_state = module.snapshot(self.compose)
                self.assertNotEqual(host_state["namespace"], bridge_state["namespace"])

    def test_snapshot_refuses_missing_or_multiple_services(self):
        for output in ("", "one\ntwo"):
            with patch.object(module,"run",return_value=output):
                with self.assertRaises(module.ReconcileError):
                    module.snapshot(self.compose)

if __name__ == "__main__":
    unittest.main()
