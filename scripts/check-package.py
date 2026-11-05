"""Check unsigned and explicitly signed packages without using release keys."""
import base64
import json
from pathlib import Path
import subprocess
import tempfile
import zipfile


def run(*argv):
    return subprocess.run([str(arg) for arg in argv], check=True)


def reject(label, *argv, reason):
    result = subprocess.run([str(arg) for arg in argv], capture_output=True, text=True)
    if result.returncode == 0 or reason not in result.stderr:
        raise SystemExit(f"Expected rejection failed: {label}\n{result.stdout}{result.stderr}")
    print(f"Expected rejection passed: {label}")


def rewrite_archive(source, destination, replacements=None, omit=()):
    replacements = dict(replacements or {})
    with zipfile.ZipFile(source) as original, zipfile.ZipFile(
        destination, "w", compression=zipfile.ZIP_DEFLATED
    ) as changed:
        for info in original.infolist():
            if info.filename not in omit:
                changed.writestr(
                    info, replacements.pop(info.filename, original.read(info.filename))
                )
        for name, contents in replacements.items():
            changed.writestr(name, contents)


root = Path(__file__).resolve().parents[1]
version = (root / "VERSION").read_text().strip()
if Path.cwd().resolve() != root:
    raise SystemExit("Run this check from the ModelGuard project root.")

build = root / "build"
build.mkdir(exist_ok=True)
with tempfile.TemporaryDirectory(prefix=".package-check-", dir=build) as directory:
    work = Path(directory)
    packager, verifier = work / "package", work / "verify-package"
    run("go", "build", "-o", packager, "./cmd/package")
    run("go", "build", "-o", verifier, "./cmd/verify-package")

    unsigned = work / "unsigned" / f"ModelGuard-{version}.s2plugin"
    unsigned.parent.mkdir()
    # An unrelated, existing public-key sidecar must not be read or overwritten.
    old_public = unsigned.parent / "publisher-public-key.txt"
    old_public.write_text("existing-public-key-sidecar\n")
    run(packager, "-out", unsigned)
    if old_public.read_text() != "existing-public-key-sidecar\n":
        raise SystemExit("Unsigned packaging changed an existing public key.")

    signed = work / "signed" / f"ModelGuard-{version}.s2plugin"
    seed = signed.parent / "disposable-publisher.seed"
    public = signed.parent / "publisher-public-key.txt"
    run(packager, "-key", seed, "-key-id", "modelguard-test-only", "-generate-key", "-out", signed)

    for archive, signature_status, extra in (
        (unsigned, "unsigned", ["-allow-unsigned", "-public-key", work / "absent-public-key.txt"]),
        (signed, "passed", ["-public-key", public]),
    ):
        result = subprocess.run(
            [str(arg) for arg in [verifier, "-archive", archive, *extra]],
            check=True, capture_output=True, text=True,
        )
        receipt = json.loads(result.stdout)
        if receipt["signature"] != signature_status or receipt["file_hashes"] != "passed":
            raise SystemExit("Package verification returned the wrong signature or integrity state.")
        print(result.stdout, end="")
        with zipfile.ZipFile(archive) as package:
            has_signature = "signature.json" in package.namelist()
            if has_signature != (signature_status == "passed"):
                raise SystemExit("Archive signature presence differs from requested package mode.")
            raw_manifest = package.read("manifest.json")
            manifest = json.loads(raw_manifest)
            if manifest["version"] != version:
                raise SystemExit("Package version differs from VERSION.")
            if raw_manifest != (archive.parent / "manifest.json").read_bytes():
                raise SystemExit("Manifest sidecar differs from the actual archive.")
            for name in (
                "ui/index.html", "ui/app.js", "ui/style.css",
                "docs/API-BRIDGE.md", "scripts/api-credential-bridge.py",
                "scripts/api-bridge-readonly.sql", "scripts/api-bridge.Dockerfile",
                "scripts/modelguard-api-bridge.service",
            ):
                if package.read(name) != (root / name).read_bytes():
                    raise SystemExit(f"Package does not contain current source: {name}")

    run(
        "node", "--input-type=module", "-e",
        """import fs from 'node:fs';
import Ajv from 'ajv/dist/2020.js';
const schema=JSON.parse(fs.readFileSync('internal/pluginv1/manifest.schema.json','utf8'));
const validate=new Ajv({strict:false}).compile(schema);
for(const filename of process.argv.slice(1)){
 const manifest=JSON.parse(fs.readFileSync(filename,'utf8'));
 if(!validate(manifest)){console.error(validate.errors);process.exit(1)}
}
console.log('Unsigned and signed package manifest schemas passed.');""",
        unsigned.parent / "manifest.json", signed.parent / "manifest.json",
    )

    reject("unsigned requires an explicit verifier opt-in", verifier, "-archive", unsigned, reason="unsigned package")
    tampered = work / "tampered.s2plugin"
    rewrite_archive(unsigned, tampered, {"ui/app.js": (root / "ui/app.js").read_bytes() + b"\n// changed\n"})
    reject("changed payload", verifier, "-archive", tampered, "-allow-unsigned", reason="hash mismatch")
    missing = work / "missing.s2plugin"
    rewrite_archive(unsigned, missing, omit={"LICENSE"})
    reject("missing declared file", verifier, "-archive", missing, "-allow-unsigned", reason="missing declared file")
    extra = work / "extra.s2plugin"
    rewrite_archive(unsigned, extra, {"undeclared.txt": b"unexpected"})
    reject("undeclared payload", verifier, "-archive", extra, "-allow-unsigned", reason="undeclared archive entry")
    with zipfile.ZipFile(unsigned) as package:
        manifest = json.loads(package.read("manifest.json"))
    manifest["schema_version"] = 0
    invalid_manifest = work / "invalid-manifest.s2plugin"
    rewrite_archive(unsigned, invalid_manifest, {"manifest.json": json.dumps(manifest).encode()})
    reject("invalid basic manifest", verifier, "-archive", invalid_manifest, "-allow-unsigned", reason="invalid basic manifest fields")

    with zipfile.ZipFile(signed) as package:
        signature = json.loads(package.read("signature.json"))
    signature_bytes = bytearray(base64.b64decode(signature["signature"]))
    signature_bytes[0] ^= 1
    signature["signature"] = base64.b64encode(signature_bytes).decode()
    bad_signature = work / "bad-signature.s2plugin"
    rewrite_archive(signed, bad_signature, {"signature.json": json.dumps(signature).encode()})
    reject("bad signature is never unsigned", verifier, "-archive", bad_signature,
           "-public-key", public, "-allow-unsigned", reason="signature verification failed")

    failed_output = work / "must-not-exist.s2plugin"
    reject("missing explicit signing key", packager, "-key", work / "absent.seed", "-out", failed_output, reason="absent.seed")
    invalid_key = work / "invalid.seed"
    invalid_key.write_bytes(b"not an Ed25519 seed")
    reject("invalid explicit signing key", packager, "-key", invalid_key, "-out", failed_output, reason="invalid signing seed")
    reject("empty explicit signing key", packager, "-key", "", "-out", failed_output, reason="-key must name")
    reject("key generation requires a destination", packager, "-generate-key", "-out", failed_output, reason="-key must name")
    old_seed = seed.read_bytes()
    reject("key generation never replaces an existing key", packager, "-key", seed,
           "-generate-key", "-out", failed_output, reason="file exists")
    if failed_output.exists() or seed.read_bytes() != old_seed:
        raise SystemExit("A rejected signing operation produced output or replaced the key.")

print("Unsigned and signed package checks passed; temporary packages and test signing seed removed.")
