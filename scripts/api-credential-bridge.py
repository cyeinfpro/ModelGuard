"""Local ModelGuard credential bridge. Unix socket only; never logs credentials."""
from __future__ import annotations

import concurrent.futures
import contextlib
from dataclasses import dataclass
import errno
import fcntl
import hmac
import http.server
import importlib
import ipaddress
import json
import os
from pathlib import Path
import re
import queue
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
    db_mode: str = "direct"
    db_host: str = "/var/run/postgresql"
    db_port: int = 5432
    db_name: str = "sub2api"
    db_user: str = "modelguard_bridge"
    db_passfile: str = ""
    db_sslmode: str = "prefer"


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


def local_admin_url(value: str) -> str:
    parsed = urllib.parse.urlsplit(value)
    host = (parsed.hostname or "").lower()
    try:
        address = None if host == "localhost" else ipaddress.ip_address(host)
        local = host == "localhost" or ("%" not in host and (
            address.is_loopback or getattr(address, "ipv4_mapped", None)
            and address.ipv4_mapped.is_loopback))
    except ValueError:
        local = False
    try:
        port = parsed.port
    except ValueError:
        raise ValueError("management URL has an invalid port") from None
    if (parsed.scheme not in ("http", "https") or not local
            or parsed.username is not None or parsed.password is not None
            or parsed.path not in ("", "/") or parsed.query or parsed.fragment
            or parsed.netloc.endswith(":") or port == 0):
        raise ValueError("management URL must be a loopback site root")
    return parsed._replace(path="").geturl()


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
    )
    admin_url = local_admin_url(admin_url)
    mode = os.environ.get("MODEL_GUARD_DB_MODE", "direct")
    if mode not in ("direct", "docker"):
        raise ValueError("unsupported database connection mode")
    port = int(os.environ.get("MODEL_GUARD_DB_PORT", "5432"))
    if not 1 <= port <= 65535:
        raise ValueError("invalid database port")
    sslmode = os.environ.get("MODEL_GUARD_DB_SSLMODE", "prefer")
    if sslmode not in ("disable", "allow", "prefer", "require", "verify-ca", "verify-full"):
        raise ValueError("invalid database TLS mode")
    return Config(
        root=root, token=token, admin_url=admin_url,
        db_container=os.environ.get("MODEL_GUARD_DB_CONTAINER", "sub2api-postgres"),
        uid=int(os.environ.get("MODEL_GUARD_UID", "1000")),
        gid=int(os.environ.get("MODEL_GUARD_GID", "1000")),
        db_mode=mode,
        db_host=os.environ.get("MODEL_GUARD_DB_HOST", "/var/run/postgresql"),
        db_port=port,
        db_name=os.environ.get("MODEL_GUARD_DB_NAME", "sub2api"),
        db_user=os.environ.get("MODEL_GUARD_DB_USER", "modelguard_bridge"),
        db_passfile=os.environ.get("MODEL_GUARD_DB_PASSFILE", ""),
        db_sslmode=sslmode,
    )


class DatabasePool:
    """Bounded direct connections; one borrower per connection, no Docker access."""

    def __init__(self, config: Config, limit: int = 8):
        self.config = config
        self.limit = limit
        self.idle = queue.LifoQueue(limit)
        self.lock = threading.Lock()
        self.count = 0
        self.closed = False

    def connect(self):
        # Imported lazily: healthz and synthetic tests need no DB dependency.
        driver = importlib.import_module("psycopg")
        c = self.config
        kwargs = dict(host=c.db_host, port=c.db_port, dbname=c.db_name,
                      user=c.db_user, sslmode=c.db_sslmode, connect_timeout=5,
                      autocommit=True, application_name="modelguard-bridge",
                      options="-c default_transaction_read_only=on -c statement_timeout=8000")
        if c.db_passfile:
            kwargs["passfile"] = c.db_passfile
        return driver.connect(**kwargs)

    def acquire(self):
        with self.lock:
            if self.closed:
                raise BridgeError(503, "database pool is closed")
            try:
                return self.idle.get_nowait()
            except queue.Empty:
                create = self.count < self.limit
                if create:
                    self.count += 1
        if create:
            try:
                return self.connect()
            except Exception:
                with self.lock:
                    self.count -= 1
                raise
        try:
            return self.idle.get(timeout=1)
        except queue.Empty:
            raise BridgeError(503, "database connection limit reached") from None

    def release(self, connection, broken=False):
        with self.lock:
            if broken or self.closed or connection.closed:
                self.count -= 1
                connection.close()
            else:
                self.idle.put_nowait(connection)

    @contextlib.contextmanager
    def connection(self):
        connection = self.acquire()
        broken = False
        try:
            yield connection
        except Exception:
            broken = True
            raise
        finally:
            self.release(connection, broken)

    def close(self):
        with self.lock:
            self.closed = True
            while not self.idle.empty():
                self.idle.get_nowait().close()
                self.count -= 1


DATABASE_POOLS = {}
DATABASE_POOL_LOCK = threading.Lock()


def database_pool(config: Config) -> DatabasePool:
    # Do not include the socket token in a cache key or diagnostic value.
    key = (config.db_host, config.db_port, config.db_name, config.db_user,
           config.db_passfile, config.db_sslmode)
    with DATABASE_POOL_LOCK:
        if key not in DATABASE_POOLS:
            DATABASE_POOLS[key] = DatabasePool(config)
        return DATABASE_POOLS[key]


def close_database_pools():
    with DATABASE_POOL_LOCK:
        pools = list(DATABASE_POOLS.values())
        DATABASE_POOLS.clear()
    for pool in pools:
        pool.close()


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
        exc.close()
        raise BridgeError(
            status_code, "management authorization or account lookup failed"
        ) from None


ACCOUNT_SQL = """select json_build_object(
    'id',id,'name',name,'platform',platform,'type',type,
    'status',status,'schedulable',schedulable,'updated_at',updated_at,
    'expires_at',expires_at,'parent_account_id',parent_account_id,
    'credentials',json_build_object('api_key',api_key,'base_url',base_url,
        'model_mapping',model_mapping),'proxy',proxy)
    from public.modelguard_bridge_accounts where id=%s"""


def database_account(config: Config, account_id: int) -> dict:
    if type(account_id) is not int or not 0 < account_id < 10 ** 18:
        raise BridgeError(404, "not found")
    if config.db_mode == "docker":
        # Explicit migration fallback only. The default direct mode never starts
        # a subprocess or needs Docker access. The same projected view and
        # configured readonly role are used in either mode.
        raw = subprocess.run(
            ["docker", "exec", config.db_container, "psql", "-X", "-v",
             "ON_ERROR_STOP=1", "-U", config.db_user, "-d", config.db_name,
             "-Atc", ACCOUNT_SQL.replace("%s", str(account_id))],
            capture_output=True, timeout=12, check=True,
        ).stdout
        if not raw.strip():
            raise BridgeError(404, "account not found")
        if len(raw) > MAX_RESPONSE_BYTES:
            raise BridgeError(502, "account response too large")
        account = json.loads(raw)
    else:
        with database_pool(config).connection() as connection:
            with connection.execute(ACCOUNT_SQL, (account_id,)) as cursor:
                row = cursor.fetchone()
        if not row:
            raise BridgeError(404, "account not found")
        account = row[0]
        if len(json.dumps(account).encode()) > MAX_RESPONSE_BYTES:
            raise BridgeError(502, "account response too large")
    if (not isinstance(account, dict) or account.get("id") != account_id
            or account.get("platform") != "openai"
            or account.get("type") != "apikey"
            or account.get("parent_account_id") is not None
            or not account.get("updated_at")
            or not isinstance(account.get("credentials"), dict)):
        raise BridgeError(409, "account snapshot changed")
    # Never forward extra credential keys even if a DBA replaces the view.
    account["credentials"] = {
        key: value for key, value in account["credentials"].items()
        if key in ("api_key", "base_url", "model_mapping")
    }
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
        if self.path == "/healthz":
            return self.reply(200, {"ok": True})
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
        close_database_pools()


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
            if (owned_socket.st_uid, owned_socket.st_gid) != (config.uid, config.gid):
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
