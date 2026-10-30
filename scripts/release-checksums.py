#!/usr/bin/env python3
"""Write checksums for the current package and only its applicable public key."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]
VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?")


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_checksums(root):
    version = (root / "VERSION").read_text(encoding="utf-8").strip()
    if not VERSION_PATTERN.fullmatch(version):
        raise ValueError("VERSION must be a single semantic version.")
    package = root / "dist" / f"ModelGuard-{version}.s2plugin"
    if package.is_symlink() or not package.is_file():
        raise ValueError("Build the current release package before creating checksums.")
    with zipfile.ZipFile(package) as archive:
        manifest = json.loads(archive.read("manifest.json"))
        if manifest.get("version") != version:
            raise ValueError("Package manifest version differs from VERSION.")
        signed = "signature.json" in archive.namelist()
    assets = [package]
    if signed:
        public_key = package.parent / "publisher-public-key.txt"
        if public_key.is_symlink() or not public_key.is_file():
            raise ValueError("The signed package is missing its public-key file.")
        assets.append(public_key)
    # An old public key may remain beside an unsigned build; it is deliberately
    # excluded so the release cannot imply a signature the package lacks.
    contents = "".join(f"{sha256_file(path)}  {path.name}\n" for path in assets)
    destination = package.parent / "SHA256SUMS"
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", newline="\n",
                                         dir=package.parent, prefix=".SHA256SUMS-",
                                         delete=False) as stream:
            temporary = Path(stream.name)
            stream.write(contents)
        os.replace(temporary, destination)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)
    return destination, len(assets), signed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    try:
        path, count, signed = write_checksums(ROOT)
    except (OSError, ValueError, KeyError, zipfile.BadZipFile) as error:
        parser.exit(1, f"Release checksum generation failed: {error}\n")
    print(f"Wrote {path.relative_to(ROOT)} for {count} assets ({'signed' if signed else 'unsigned'} package).")


if __name__ == "__main__":
    main()
