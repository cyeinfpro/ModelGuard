"""Bridge regressions; isolated Unix/TCP fixtures, no Docker or real accounts."""
import contextlib
import dataclasses
import errno
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import socket
import sys
import tempfile
import threading
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "modelguard_bridge", Path(__file__).resolve().parents[1] / "scripts/api-credential-bridge.py"
)
bridge = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = bridge
spec.loader.exec_module(bridge)


class BridgeTests(unittest.TestCase):
    def setUp(self):
        # A short local path works on macOS and when the project is on SMB.
        self.tmp = tempfile.TemporaryDirectory(prefix="mg-", dir="/tmp")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.config = bridge.Config(self.root, "b" * 64)
        self.account = {
            "id": 7, "platform": "openai", "type": "apikey", "status": "active",
            "schedulable": True, "updated_at": "2026-10-30T12:00:00Z",
            "credentials": {"api_key": "secret-upstream-value"},
        }

    def test_authorization_is_rechecked_after_database_read(self):
        with patch.object(bridge, "lookup_scope", side_effect=[
            self.account, bridge.BridgeError(403, "scope revoked")
        ]) as scope, patch.object(bridge, "database_account", return_value=self.account) as database:
            with self.assertRaises(bridge.BridgeError) as error:
                bridge.read_account(self.config, 7, "scoped-key")
            self.assertEqual(error.exception.status, 403)
            self.assertEqual(scope.call_count, 2)
            database.assert_called_once()

    def test_account_changes_abort_secret_return(self):
        for changed in (
            dict(self.account, schedulable=False),
            dict(self.account, status="disabled"),
            dict(self.account, updated_at="2026-10-30T12:00:01Z"),
        ):
            with self.subTest(changed=changed["status"]):
                with patch.object(bridge, "lookup_scope", side_effect=[self.account, changed]), \
                     patch.object(bridge, "database_account", return_value=self.account):
                    with self.assertRaises(bridge.BridgeError) as error:
                        bridge.read_account(self.config, 7, "scoped-key")
                    self.assertEqual(error.exception.status, 409)

    def test_valid_scoped_snapshot_returns_account(self):
        with patch.object(bridge, "lookup_scope", return_value=self.account), \
             patch.object(bridge, "database_account", return_value=self.account):
            self.assertEqual(bridge.read_account(self.config, 7, "key"), self.account)

    def test_invalid_identifiers_never_reach_database(self):
        with patch.object(bridge, "lookup_scope") as scope:
            for invalid in (0, -1, True, "7; select", 10 ** 18):
                with self.assertRaises(bridge.BridgeError):
                    bridge.read_account(self.config, invalid, "key")
            scope.assert_not_called()

    def test_management_redirect_does_not_forward_key(self):
        seen = []

        class Redirect(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                seen.append((self.path, self.headers.get("x-api-key")))
                self.send_response(302)
                self.send_header("Location", "/secret-sink")
                self.end_headers()

        with http.server.ThreadingHTTPServer(("127.0.0.1", 0), Redirect) as server:
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            config = dataclasses.replace(self.config, admin_url=f"http://127.0.0.1:{server.server_port}")
            try:
                with self.assertRaises(bridge.BridgeError) as error:
                    bridge.lookup_scope(config, 7, "must-not-redirect")
                self.assertEqual(error.exception.status, 502)
                self.assertEqual(seen, [("/api/v1/admin/accounts/7", "must-not-redirect")])
            finally:
                server.shutdown()
                worker.join(2)

    def test_management_error_envelope_or_wrong_scope_is_rejected(self):
        for payload, expected in (
            ({"code": 1, "data": self.account}, 502),
            ({"code": 0, "data": dict(self.account, platform="anthropic")}, 403),
            ({"code": 0, "data": dict(self.account, parent_account_id=1)}, 403),
            ({"code": 0, "data": dict(self.account, schedulable="true")}, 403),
        ):
            response = io.BytesIO(json.dumps(payload).encode())
            with patch.object(bridge.ADMIN_HTTP, "open", return_value=response):
                with self.assertRaises(bridge.BridgeError) as error:
                    bridge.lookup_scope(self.config, 7, "key")
                self.assertEqual(error.exception.status, expected)

    def test_database_wrong_account_is_rejected(self):
        output = types.SimpleNamespace(stdout=json.dumps(dict(self.account, id=8)).encode())
        with patch.object(bridge.subprocess, "run", return_value=output):
            with self.assertRaises(bridge.BridgeError) as error:
                bridge.database_account(dataclasses.replace(self.config, db_mode="docker"), 7)
            self.assertEqual(error.exception.status, 409)

    def test_configuration_rejects_empty_token_and_remote_management(self):
        token = self.root / "api-bridge.token"
        with patch.dict(os.environ, {"MODEL_GUARD_DATA_DIR": str(self.root)}, clear=True):
            token.write_text("")
            with self.assertRaises(ValueError):
                bridge.load_config()
            token.write_text("t" * 64)
            self.assertEqual(bridge.load_config().token, "t" * 64)
            with patch.dict(os.environ, {"MODEL_GUARD_ADMIN_URL": "https://example.invalid"}):
                with self.assertRaises(ValueError):
                    bridge.load_config()

    def test_live_socket_and_non_socket_files_are_preserved(self):
        path = self.root / "guard.sock"
        path.write_text("user-data")
        with self.assertRaises(RuntimeError):
            bridge.prepare_socket(path)
        self.assertEqual(path.read_text(), "user-data")
        path.unlink()
        with socket.socket(socket.AF_UNIX) as listener:
            self.bind_socket(listener, path)
            listener.listen()
            with self.assertRaises(RuntimeError):
                bridge.prepare_socket(path)
            self.assertTrue(path.exists())

    def test_stale_socket_is_cleaned(self):
        path = self.root / "stale.sock"
        with socket.socket(socket.AF_UNIX) as stale:
            self.bind_socket(stale, path)
        bridge.prepare_socket(path)
        self.assertFalse(path.exists())

    @contextlib.contextmanager
    def running_bridge(self):
        path = self.root / "http.sock"
        try:
            server = bridge.Server(path, self.config)
        except OSError as exc:
            self.skip_unsupported_socket(exc)
            raise
        with server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                yield path
            finally:
                server.shutdown()
                thread.join(2)

    def skip_unsupported_socket(self, exc):
        if exc.errno in (errno.EOPNOTSUPP, errno.ENOTSUP, errno.EAFNOSUPPORT):
            self.skipTest("local temporary filesystem does not support Unix sockets")

    def bind_socket(self, sock, path):
        try:
            sock.bind(str(path))
        except OSError as exc:
            self.skip_unsupported_socket(exc)
            raise

    def request(self, path, token=None, endpoint="/accounts/7", admin_key="scoped-key"):
        headers = f"x-admin-key: {admin_key}\r\n" if admin_key else ""
        if token is not None:
            headers += f"x-bridge-token: {token}\r\n"
        with socket.socket(socket.AF_UNIX) as client:
            client.settimeout(2)
            client.connect(str(path))
            client.sendall(f"GET {endpoint} HTTP/1.0\r\n{headers}\r\n".encode())
            chunks = []
            while chunk := client.recv(65536):
                chunks.append(chunk)
        head, body = b"".join(chunks).split(b"\r\n\r\n", 1)
        return int(head.split()[1]), head, json.loads(body)

    def test_http_auth_precedes_any_account_access(self):
        with patch.object(bridge, "read_account") as read, self.running_bridge() as path:
            status, _, _ = self.request(path)
            self.assertEqual(status, 403)
            read.assert_not_called()

    def test_healthz_requires_token_but_never_reads_accounts_or_database(self):
        with patch.object(bridge, "read_account") as read, \
             patch.object(bridge, "database_account") as database, self.running_bridge() as path:
            status, _, body = self.request(path, endpoint="/healthz", admin_key=None)
            self.assertEqual(status, 403)
            status, head, body = self.request(path, self.config.token, "/healthz", None)
            self.assertEqual(status, 200)
            self.assertEqual(body, {"ok": True})
            self.assertIn(b"Cache-Control: no-store", head)
            read.assert_not_called()
            database.assert_not_called()

    def test_loopback_management_accepts_any_valid_port_and_ipv6(self):
        for url in ("http://127.0.0.1:18443", "http://[::1]:9000",
                    "https://localhost:8443/", "http://127.20.1.2:9090", "http://[::ffff:127.0.0.1]:8080"):
            self.assertEqual(bridge.local_admin_url(url), url.rstrip("/"))
        for url in ("http://127.0.0.1:0", "http://127.0.0.1:65536", "http://localhost:",
                    "http://localhost@example.invalid", "http://[::1]/api",
                    "http://127.0.0.1?admin=1", "http://127.0.0.1#fragment",
                    "http://localhost//", "http://[::1%lo]:8080"):
            with self.assertRaises(ValueError, msg=url):
                bridge.local_admin_url(url)

    def test_direct_database_is_parameterized_pooled_and_drops_extra_secrets(self):
        queries = []
        account = dict(self.account, credentials={"api_key": "needed", "refresh_token": "never-return"})

        class Connection:
            closed = False

            def execute(self, sql, params):
                queries.append((sql, params))
                cursor = types.SimpleNamespace(fetchone=lambda: (dict(account),))
                return contextlib.nullcontext(cursor)

            def close(self):
                self.closed = True

        self.addCleanup(bridge.close_database_pools)
        with patch.object(bridge.DatabasePool, "connect", side_effect=Connection) as connect, \
             patch.object(bridge.subprocess, "run") as subprocess_run:
            for _ in range(2):
                result = bridge.database_account(self.config, 7)
                self.assertEqual(result["credentials"], {"api_key": "needed"})
            self.assertEqual(connect.call_count, 1)
            self.assertEqual(len(queries), 2)
            self.assertEqual(queries[0][1], (7,))
            self.assertIn("public.modelguard_bridge_accounts", queries[0][0])
            self.assertNotIn("a.credentials", queries[0][0])
            subprocess_run.assert_not_called()

    def test_database_pool_discards_broken_connection_and_stays_bounded(self):
        class Connection:
            closed = False

            def close(self):
                self.closed = True

        pool = bridge.DatabasePool(self.config, limit=1)
        self.addCleanup(pool.close)
        with patch.object(pool, "connect", side_effect=Connection) as connect:
            first = pool.acquire()
            self.assertEqual(pool.count, 1)
            pool.release(first, broken=True)
            self.assertTrue(first.closed)
            self.assertEqual(pool.count, 0)
            second = pool.acquire()
            pool.release(second)
            self.assertIs(pool.acquire(), second)
            pool.release(second)
            self.assertEqual(connect.call_count, 2)

    def test_legacy_database_mode_requires_opt_in_and_uses_configured_role(self):
        config = dataclasses.replace(self.config, db_mode="docker", db_user="readonly", db_name="custom")
        result = types.SimpleNamespace(stdout=json.dumps(self.account).encode())
        with patch.object(bridge.subprocess, "run", return_value=result) as run:
            bridge.database_account(config, 7)
            command = run.call_args.args[0]
            self.assertEqual(command[command.index("-U") + 1], "readonly")
            self.assertEqual(command[command.index("-d") + 1], "custom")
            self.assertIn("modelguard_bridge_accounts", command[-1])

    def test_http_internal_errors_are_redacted(self):
        with patch.object(bridge, "read_account", side_effect=RuntimeError("secret-upstream-value")), \
             self.running_bridge() as path:
            status, _, body = self.request(path, self.config.token)
            self.assertEqual(status, 503)
            self.assertNotIn("secret-upstream-value", json.dumps(body))

    def test_http_success_disallows_caching(self):
        with patch.object(bridge, "read_account", return_value=self.account), self.running_bridge() as path:
            status, head, body = self.request(path, self.config.token)
            self.assertEqual(status, 200)
            self.assertIn(b"Cache-Control: no-store", head)
            self.assertEqual(body["id"], 7)

    def test_pending_connection_queue_is_bounded(self):
        entered, release = threading.Event(), threading.Event()
        pairs = [socket.socketpair() for _ in range(3)]
        self.addCleanup(lambda: [sock.close() for pair in pairs for sock in pair])
        try:
            server = bridge.Server(self.root / "bounded.sock", self.config, max_workers=1, max_pending=1)
        except OSError as exc:
            self.skip_unsupported_socket(exc)
            raise
        with server:
            def wait_for_release(*_):
                entered.set()
                release.wait(2)
            with patch.object(server, "finish_request", side_effect=wait_for_release):
                try:
                    server.process_request(pairs[0][0], "")
                    self.assertTrue(entered.wait(1))
                    server.process_request(pairs[1][0], "")
                    server.process_request(pairs[2][0], "")
                    pairs[2][1].settimeout(1)
                    self.assertEqual(pairs[2][1].recv(1), b"")
                finally:
                    # A queued task may start as the mock context exits. Closing
                    # peers also bounds a real handler's read in that race.
                    pairs[0][1].close()
                    pairs[1][1].close()
                    release.set()


if __name__ == "__main__":
    unittest.main()
