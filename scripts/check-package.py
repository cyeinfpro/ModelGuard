"""Exercise the current packager with a disposable test publisher, never release keys."""
import json
from pathlib import Path
import subprocess
import tempfile
import zipfile


def run(*argv):
    subprocess.run(argv, check=True)


root = Path(__file__).resolve().parents[1]
version = (root / "VERSION").read_text().strip()
if Path.cwd().resolve() != root:
    raise SystemExit("Run this check from the ModelGuard project root.")

build = root / "build"
build.mkdir(exist_ok=True)
with tempfile.TemporaryDirectory(prefix=".package-check-", dir=build) as work:
    work = Path(work)
    archive = work / f"ModelGuard-{version}.s2plugin"
    run(
        "go", "run", "./cmd/package",
        "-key", str(work / "disposable-publisher.seed"),
        "-key-id", "modelguard-test-only",
        "-generate-key", "-out", str(archive),
    )
    run(
        "go", "run", "./cmd/verify-package",
        "-archive", str(archive),
        "-public-key", str(work / "publisher-public-key.txt"),
    )
    with zipfile.ZipFile(archive) as package:
        manifest = json.loads(package.read("manifest.json"))
        if manifest["version"] != version:
            raise SystemExit("Package version differs from VERSION.")
        for name in (
            "ui/index.html", "ui/app.js", "ui/style.css",
            "docs/API-BRIDGE.md", "scripts/api-credential-bridge.py",
        ):
            if package.read(name) != (root / name).read_bytes():
                raise SystemExit(f"Package does not contain current source: {name}")
    run(
        "node", "--input-type=module", "-e",
        """import fs from 'node:fs';
import Ajv from 'ajv/dist/2020.js';
const schema=JSON.parse(fs.readFileSync('internal/pluginv1/manifest.schema.json','utf8'));
const manifest=JSON.parse(fs.readFileSync(process.argv[1],'utf8'));
const validate=new Ajv({strict:false}).compile(schema);
if(!validate(manifest)){console.error(validate.errors);process.exit(1)}
console.log('Current package schema, version, source files, signature and hashes passed.');""",
        str(work / "manifest.json"),
    )
print("Disposable test package and signing seed removed; no release package was published.")
