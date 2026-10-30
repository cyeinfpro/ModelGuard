#!/usr/bin/env python3
"""Publish a verified tag without replacing conflicting GitHub release assets."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.parse import quote
import zipfile


ROOT = Path(__file__).resolve().parents[1]
VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?")


def run(*args, root=ROOT, allow_missing=False, output=None):
    result = subprocess.run(args, cwd=root, stdout=output or subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=300)
    if result.returncode:
        error = result.stderr.decode("utf-8", errors="replace").strip()
        if allow_missing and re.search(r"\bHTTP 404\b", error):
            return None
        raise RuntimeError(f"{args[0]} failed: {error or 'nonzero exit status'}")
    if output is None:
        return result.stdout.decode("utf-8").strip()
    return ""


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def release_assets(root, version):
    directory = root / "dist"
    checksum_file = directory / "SHA256SUMS"
    if checksum_file.is_symlink() or not checksum_file.is_file():
        raise ValueError("dist/SHA256SUMS is missing; build and verify the package first.")
    package_name = f"ModelGuard-{version}.s2plugin"
    allowed = {package_name, "publisher-public-key.txt"}
    assets = {}
    for line in checksum_file.read_text(encoding="utf-8").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  (.+)", line)
        if not match or match[2] not in allowed or match[2] in assets:
            raise ValueError("SHA256SUMS contains an invalid, unexpected, or duplicate asset.")
        name, expected = match[2], match[1]
        path = directory / name
        if path.is_symlink() or not path.is_file() or sha256_file(path) != expected:
            raise ValueError(f"Release asset checksum mismatch: {name}")
        assets[name] = (path, expected)
    if package_name not in assets:
        raise ValueError("SHA256SUMS does not include the current version package.")
    with zipfile.ZipFile(directory / package_name) as package:
        manifest = json.loads(package.read("manifest.json"))
        if manifest.get("version") != version:
            raise ValueError("Package manifest version differs from VERSION.")
        signed = "signature.json" in package.namelist()
    if signed != ("publisher-public-key.txt" in assets):
        raise ValueError("Release public-key asset does not match the package signing mode.")
    assets[checksum_file.name] = (checksum_file, sha256_file(checksum_file))
    return assets


def get_release(repository, tag):
    raw = run("gh", "api", f"repos/{repository}/releases/tags/{quote(tag, safe='')}",
              allow_missing=True)
    return json.loads(raw) if raw is not None else None


def remote_asset_hash(repository, asset):
    if asset.get("state") != "uploaded":
        raise ValueError(f"Existing release asset is not fully uploaded: {asset['name']}")
    digest = asset.get("digest") or ""
    if re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        return digest.removeprefix("sha256:")
    # Older assets have no digest metadata; hash their actual bytes instead.
    with tempfile.TemporaryDirectory(prefix="modelguard-release-check-") as work:
        path = Path(work) / "asset"
        with path.open("wb") as stream:
            run("gh", "api", f"repos/{repository}/releases/assets/{int(asset['id'])}",
                "-H", "Accept: application/octet-stream", output=stream)
        return sha256_file(path)


def missing_assets(repository, release, assets):
    existing = {}
    for asset in release.get("assets", []):
        name = asset.get("name")
        if name in existing:
            raise ValueError(f"Duplicate remote release asset: {name}")
        existing[name] = asset
    missing = []
    for name, (path, expected) in assets.items():
        if name not in existing:
            missing.append(path)
        elif remote_asset_hash(repository, existing[name]) != expected:
            raise ValueError(f"Existing release asset differs: {name}; refusing to overwrite it.")
        else:
            print(f"Verified existing asset: {name}")
    return missing


def publish(tag):
    version = (ROOT / "VERSION").read_text(encoding="utf-8").strip()
    if not VERSION_PATTERN.fullmatch(version) or tag != f"v{version}":
        raise ValueError("Release tag must be exactly v<VERSION>.")
    head = run("git", "rev-parse", "--verify", "HEAD")
    tagged = run("git", "rev-parse", "--verify", f"refs/tags/{tag}^{{commit}}")
    if head != tagged:
        raise ValueError("HEAD differs from the release tag; check out the exact tagged commit.")
    assets = release_assets(ROOT, version)
    repository = run("gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("Could not resolve the current GitHub repository.")
    remote_commit = run("gh", "api", f"repos/{repository}/commits/{quote(tag, safe='')}", "--jq", ".sha")
    if remote_commit != head:
        raise ValueError("The remote tag differs from HEAD; refusing to publish or move it.")
    release = get_release(repository, tag)
    if release is None:
        run("gh", "release", "create", tag, "--repo", repository, "--verify-tag", "--draft",
            "--generate-notes", "--title", f"ModelGuard v{version}")
        release = get_release(repository, tag)
    if not release or release.get("tag_name") != tag:
        raise ValueError("Could not verify the release for the requested tag.")
    pending = missing_assets(repository, release, assets)
    for path in pending:
        run("gh", "release", "upload", tag, str(path), "--repo", repository)
        print(f"Uploaded asset: {path.name}")
    release = get_release(repository, tag)
    if not release or missing_assets(repository, release, assets):
        raise ValueError("Release upload is incomplete; the draft has not been published.")
    if release.get("draft"):
        run("gh", "release", "edit", tag, "--repo", repository, "--draft=false")
    print(f"Release assets verified: https://github.com/{repository}/releases/tag/{quote(tag, safe='')}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True, help="Existing release tag; must equal v<VERSION>")
    args = parser.parse_args()
    try:
        publish(args.tag)
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired,
            zipfile.BadZipFile) as error:
        parser.exit(1, f"Release publication failed: {error}\n")


if __name__ == "__main__":
    main()
