"""Check or repair the running ModelGuard bridge from the Docker host."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys

HOST = "sub2api"
BRIDGE = "modelguard-api-bridge"


class ReconcileError(Exception):
    pass


def run(args):
    try:
        return subprocess.run(args, check=True, capture_output=True, text=True,
                              timeout=60).stdout.strip()
    except (subprocess.SubprocessError, OSError):
        # Docker/Compose errors may include host configuration. Do not echo them.
        raise ReconcileError("Docker/Compose operation failed; inspect locally") from None


def snapshot(compose):
    result = []
    for service in (HOST, BRIDGE):
        found = run(compose + ["ps", "-q", service]).splitlines()
        if len(found) != 1:
            raise ReconcileError("Expected exactly one running container per service")
        raw = run(["docker", "inspect", "--format",
                   '{"id":{{json .Id}},"pid":{{.State.Pid}},"running":{{.State.Running}},'
                   '"network":{{json .HostConfig.NetworkMode}}}', found[0]])
        item = json.loads(raw)
        if not item["running"] or item["pid"] <= 0:
            raise ReconcileError("Service is not running; no changes made")
        item["namespace"] = os.readlink("/proc/%s/ns/net" % item["pid"])
        result.append(item)
    if not result[1]["network"].startswith("container:"):
        raise ReconcileError("Bridge does not use a shared container network")
    return result


def reconcile(compose, apply=False):
    before = snapshot(compose)
    if before[0]["namespace"] == before[1]["namespace"]:
        return "Network namespaces already match"
    if not apply:
        raise ReconcileError("Network namespace mismatch; use --apply to recreate the bridge")
    services = json.loads(run(compose + ["config", "--format", "json"]))["services"]
    if services.get(BRIDGE, {}).get("network_mode") != f"service:{HOST}":
        raise ReconcileError("Compose must configure the bridge to share the Sub2API service network")
    # Abort on concurrent host restarts or service replacement.
    if snapshot(compose) != before:
        raise ReconcileError("Container state changed; no changes made")
    run(compose + ["up", "-d", "--no-deps", "--force-recreate",
                   "--pull", "never", "--no-build", BRIDGE])
    after = snapshot(compose)
    if after[0] != before[0]:
        raise ReconcileError("Host changed during repair; verify on the next reconciliation")
    if after[0]["namespace"] != after[1]["namespace"]:
        raise ReconcileError("Bridge recreated but namespace verification failed")
    return "Bridge recreated and shares the running Sub2API network namespace"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project-directory", required=True)
    parser.add_argument("-f", "--file", action="append", required=True)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    try:
        directory = str(Path(args.project_directory).resolve(strict=True))
        compose = ["docker", "compose", "--project-directory", directory]
        for name in args.file:
            compose += ["-f", str(Path(name).resolve(strict=True))]
        print(reconcile(compose, args.apply))
    except (ReconcileError, OSError, ValueError, KeyError, TypeError) as exc:
        print(str(exc) if isinstance(exc, ReconcileError) else "Local verification failed",
              file=sys.stderr)
        return 1
    return 0

if __name__ == "__main__":
    sys.exit(main())
