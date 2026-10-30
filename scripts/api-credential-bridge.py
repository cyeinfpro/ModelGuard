"""Local ModelGuard credential bridge. Unix socket only; never logs credentials."""
from __future__ import annotations

import concurrent.futures
from dataclasses import dataclass
import errno
import fcntl
import hmac
import http.server
import json
import os
from pathlib import Path
import re
import signal
import socket
import socketserver
import stat
import subprocess
import threading
import urllib.error
import urllib.parse
import urllib.request

MAX_RESPONSE_BYTES = 2 << 20
ACCOUNT_PATH = re.compile(r"/accounts/([1-9][0-9]{0,17})")


@dataclass(frozen=True)
class Config:
    root: Path
    token: str
    admin_url: str = "http://127.0.0.1:8080"
    db_container: str = "sub2api-postgres"
    uid: int = 1000
    gid: int = 1000


class BridgeError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


# The management key must not follow redirects or an inherited HTTP proxy.
ADMIN_HTTP = urllib.request.build_opener(
    urllib.request.ProxyHandler({}), NoRedirect()
)


def load_config() -> Config:
    root = Path(os.environ.get(
        "MODEL_GUARD_DATA_DIR",
        "/root/sub2api-deploy/data/sub2api-account-guard",
    ))
    token = (root / "api-bridge.token").read_text().strip()
    if len(token) < 32 or len(token) > 4096 or not token.isascii():
        raise ValueError("bridge token must contain 32 to 4096 ASCII characters")
    if any(ch.isspace() for ch in token):
        raise ValueError("bridge token must not contain whitespace")
    admin_url = os.environ.get(
        "MODEL_GUARD_ADMIN_URL", "http://127.0.0.1:8080"
    ).rstrip("/")
    parsed = urllib.parse.urlsplit(admin_url)
    # This helper only reads this installation's management endpoint.
    if (parsed.scheme not in ("http", "https")
            or parsed.hostname not in ("127.0.0.1", "localhost", "::1")
            or parsed.username is not None or parsed.password is not None
            or parsed.path or parsed.query or parsed.fragment):
        raise ValueError("management URL must be a local site root")
    return Config(
        root=root, token=token, admin_url=admin_url,
        db_container=os.environ.get("MODEL_GUARD_DB_CONTAINER", "sub2api-postgres"),
        uid=int(os.environ.get("MODEL_GUARD_UID", "1000")),
        gid=int(os.environ.get("MODEL_GUARD_GID", "1000")),
    )


def lookup_scope(config: Config, account_id: int, key: str) -> dict:
    request = urllib.request.Request(
        f"{config.admin_url}/api/v1/admin/accounts/{account_id}",
        headers={"x-api-key": key},
    )
    try:
        with ADMIN_HTTP.open(request, timeout=10) as response:
            raw = response.read(MAX_RESPONSE_BYTES + 1)
        if len(raw) > MAX_RESPONSE_BYTES:
            raise BridgeError(502, "management response too large")
        envelope = json.loads(raw)
        if not isinstance(envelope, dict) or envelope.get("code") != 0:
            raise BridgeError(502, "management response invalid")
        account = envelope.get("data")
        if (not isinstance(account, dict)
                or account.get("id") != account_id
                or account.get("platform") != "openai"
                or account.get("type") != "apikey"
                or account.get("parent_account_id") is not None
                or not isinstance(account.get("status"), str)
                or type(account.get("schedulable")) is not bool):
            raise BridgeError(403, "account outside supported scope")
        return account
    except urllib.error.HTTPError as exc:
        status_code = exc.code if exc.code in (401, 403, 404, 429) else 502
        raise BridgeError(
            status_code, "management authorization or account lookup failed"
        ) from None


def database_account(config: Config, account_id: int) -> dict:
    # Interpolation is a validated positive integer. SELECT only.
    sql = """select json_build_object(
        'id',a.id,'name',a.name,'platform',a.platform,'type',a.type,
        'status',a.status,'schedulable',a.schedulable,'updated_at',a.updated_at,
        'expires_at',a.expires_at,'parent_account_id',a.parent_account_id,
        'credentials',a.credentials,
        'proxy',case when p.id is null then null else json_build_object(
            'protocol',p.protocol,'host',p.host,'port',p.port,
            'username',p.username,'password',p.password) end)
        from accounts a left join proxies p on p.id=a.proxy_id
        where a.id=%d and a.deleted_at is null
        and a.platform='openai' and a.type='apikey'""" % account_id
    raw = subprocess.run(
        ["docker", "exec", config.db_container, "psql", "-U", "sub2api",
         "-d", "sub2api", "-Atc", sql],
        capture_output=True, timeout=12, check=True,
    ).stdout
    if not raw.strip():
        raise BridgeError(404, "account not found")
    if len(raw) > MAX_RESPONSE_BYTES:
        raise BridgeError(502, "account response too large")
    account = json.loads(raw)
    if (not isinstance(account, dict) or account.get("id") != account_id
            or account.get("platform") != "openai"
            or account.get("type") != "apikey"
            or account.get("parent_account_id") is not None
            or not account.get("updated_at")
            or not isinstance(account.get("credentials"), dict)):
        raise BridgeError(409, "account snapshot changed")
    return account


def read_account(config: Config, account_id: int, key: str) -> dict:
    if type(account_id) is not int or not 0 < account_id < 10 ** 18:
        raise BridgeError(404, "not found")
    allowed = lookup_scope(config, account_id, key)
    account = database_account(config, account_id)
    # Check authorization before and after the secret snapshot. Credentials
    # only leave this process if every check succeeds.
    latest = lookup_scope(config, account_id, key)
    for snapshot in (allowed, latest):
        if (snapshot["status"] != account.get("status")
                or snapshot["schedulable"] != account.get("schedulable")):
            raise BridgeError(409, "account changed; retry")
    if (allowed.get("updated_at") and latest.get("updated_at")
            and allowed["updated_at"] != latest["updated_at"]):
        raise BridgeError(409, "account changed; retry")
    return account


class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status_code, data):
        raw = json.dumps(data).encode()
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        config = self.server.config
        supplied = self.headers.get("x-bridge-token", "")
        if not hmac.compare_digest(supplied.encode(), config.token.encode()):
            return self.reply(403, {"error": "bridge authorization required"})
        match = ACCOUNT_PATH.fullmatch(self.path)
        if not match:
            return self.reply(404, {"error": "not found"})
        key = self.headers.get("x-admin-key", "")
        if not key:
            return self.reply(403, {"error": "account scope required"})
        try:
            account = read_account(config, int(match[1]), key)
        except BridgeError as exc:
            return self.reply(exc.status, {"error": str(exc)})
        except Exception:
            # Do not return exceptions: they can contain URLs, SQL or secrets.
            return self.reply(503, {"error": "credential bridge temporarily unavailable"})
        self.reply(200, account)


class Server(socketserver.UnixStreamServer):
    def __init__(self, path, config, max_workers=8, max_pending=16):
        self.config = config
        self.pool = concurrent.futures.ThreadPoolExecutor(max_workers=max_workers)
        self.slots = threading.BoundedSemaphore(max_workers + max_pending)
        try:
            super().__init__(str(path), Handler)
        except BaseException:
            self.pool.shutdown(wait=False)
            raise

    def process_request(self, request, address):
        if not self.slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            self.pool.submit(self.process, request, address)
        except BaseException:
            self.slots.release()
            self.shutdown_request(request)
            raise

    def process(self, request, address):
        try:
            request.settimeout(40)
            self.finish_request(request, address)
        except Exception:
            pass
        finally:
            self.shutdown_request(request)
            self.slots.release()

    def server_close(self):
        super().server_close()
        self.pool.shutdown(wait=True)


def prepare_socket(path: Path):
    try:
        previous = path.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISSOCK(previous.st_mode):
        raise RuntimeError("socket path contains another file; refusing to replace it")
    with socket.socket(socket.AF_UNIX) as probe:
        probe.settimeout(1)
        try:
            probe.connect(str(path))
        except OSError as exc:
            if exc.errno not in (errno.ECONNREFUSED, errno.ENOENT):
                raise RuntimeError("socket is busy; refusing to replace it") from None
        else:
            raise RuntimeError("credential bridge is already running")
    try:
        current = path.lstat()
    except FileNotFoundError:
        return
    if (current.st_dev, current.st_ino) != (previous.st_dev, previous.st_ino):
        raise RuntimeError("socket path changed during startup")
    path.unlink()


def stop_server(*_):
    raise KeyboardInterrupt


def main():
    config = load_config()
    socket_path = config.root / "api-bridge.sock"
    lock_fd = os.open(
        config.root / "api-bridge.lock",
        os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0), 0o600,
    )
    owned_socket = None
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError("credential bridge is already running") from None
        prepare_socket(socket_path)
        with Server(socket_path, config) as server:
            owned_socket = socket_path.stat()
            os.chown(socket_path, config.uid, config.gid)
            os.chmod(socket_path, 0o600)
            signal.signal(signal.SIGTERM, stop_server)
            try:
                server.serve_forever()
            except KeyboardInterrupt:
                pass
    finally:
        if owned_socket is not None:
            try:
                current = socket_path.lstat()
                if (current.st_dev, current.st_ino) == (owned_socket.st_dev, owned_socket.st_ino):
                    socket_path.unlink()
            except FileNotFoundError:
                pass
        os.close(lock_fd)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, RuntimeError):
        # Avoid printing configuration, tokens or exception details on startup.
        raise SystemExit("Credential bridge could not start; check local configuration and socket ownership.")
