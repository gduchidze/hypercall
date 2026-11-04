#!/usr/bin/env python3
"""Build a pinned offline CPython 3.12 Linux harness wheel bundle.

Network is used only here to fetch exact locked public wheels. Image installation
uses the resulting archive without package indexes, pip, or guest network access.
"""
from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import tomllib
from urllib.request import urlopen
import zipfile

ROOT = Path(__file__).resolve().parents[1]
LOCK = ROOT / "deploy/runtime/harness-wheels.lock.json"
LAUNCHER = b'''import sys
if sys.version_info[:2] != (3, 12):
    raise RuntimeError("cooperative harness requires CPython 3.12 in this image")
sys.path.insert(0, "/opt/hypercall/python")
from hypercall.fixture_agent import main
main()
'''


def sha(data):
    return hashlib.sha256(data).hexdigest()


def fetch(entry, cache):
    destination = cache / entry["filename"]
    if not destination.exists():
        with urlopen(entry["url"], timeout=60) as response:
            data = response.read(entry["size"] + 1)
        if len(data) != entry["size"] or sha(data) != entry["sha256"]:
            raise ValueError("locked public wheel integrity mismatch")
        with tempfile.NamedTemporaryFile(dir=cache, delete=False) as temporary:
            temporary.write(data)
            temporary_name = temporary.name
        os.replace(temporary_name, destination)
    data = destination.read_bytes()
    if len(data) != entry["size"] or sha(data) != entry["sha256"]:
        raise ValueError("cached public wheel integrity mismatch")
    return data


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=("arm64", "amd64"), required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cache", type=Path, default=Path.home() / ".cache/hypercall/harness-wheels")
    args = parser.parse_args()
    if args.output.exists():
        raise SystemExit("output already exists; select a new immutable artifact path")
    args.cache.mkdir(parents=True, exist_ok=True)
    project = tomllib.loads((ROOT / "pyproject.toml").read_text())["project"]
    lock = json.loads(LOCK.read_bytes())
    entries = lock["architectures"][args.arch]
    contents = {}
    wheels = []
    for entry in entries:
        data = fetch(entry, args.cache)
        contents["wheels/" + entry["filename"]] = data
        wheels.append(dict(entry))
    with tempfile.TemporaryDirectory(prefix="hypercall-sdk-wheel-") as directory:
        subprocess.run(["uv", "build", "--no-build-isolation", "--wheel", "--out-dir", directory],
                       cwd=ROOT, env=dict(os.environ, SOURCE_DATE_EPOCH="315532800"), check=True)
        built = list(Path(directory).glob("*.whl"))
        if len(built) != 1:
            raise SystemExit("exactly one local SDK wheel required")
        data = built[0].read_bytes()
        contents["wheels/" + built[0].name] = data
        wheels.append({"distribution": "hypercall-sdk", "version": project["version"],
                       "filename": built[0].name, "sha256": sha(data), "size": len(data),
                       "url": "local-wheel-build"})
    # Preserve the complete wheel, including METADATA, WHEEL, RECORD, LICENSE
    # and NOTICE entries; install integration must not discard dist-info data.
    for name, data in contents.items():
        with zipfile.ZipFile(io.BytesIO(data)) as wheel:
            members = wheel.namelist()
            if not any(".dist-info/" in p and p.endswith("METADATA") for p in members):
                raise ValueError("wheel metadata is missing")
            if not any("license" in p.lower() or "copying" in p.lower() for p in members):
                raise ValueError("wheel license material is missing")
    manifest = {"schema_version": 1, "python_abi": "cp312", "python_version": "3.12",
                "architecture": args.arch, "sdk_version": project["version"], "wheels": wheels,
                "launcher": {"filename": "agent.py", "sha256": sha(LAUNCHER), "size": len(LAUNCHER)},
                "wheel_lock_sha256": sha(LOCK.read_bytes())}
    contents["manifest.json"] = (json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n").encode()
    contents["agent.py"] = LAUNCHER
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with args.output.open("xb") as raw, gzip.GzipFile(fileobj=raw, mode="wb", filename="", mtime=0) as compressed, tarfile.open(fileobj=compressed, mode="w") as archive:
        for name, data in sorted(contents.items()):
            info = tarfile.TarInfo(name)
            info.size, info.mode, info.mtime = len(data), 0o644, 0
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            archive.addfile(info, io.BytesIO(data))
    print(json.dumps({"artifact": str(args.output), "sha256": sha(args.output.read_bytes()),
                      "architecture": args.arch, "python_abi": "cp312"}))


if __name__ == "__main__":
    main()
