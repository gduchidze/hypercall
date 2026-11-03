#!/usr/bin/env python3
"""Generate with the compiler bundled in locked grpcio-tools and pinned Go plugins."""
from __future__ import annotations

import argparse
import filecmp
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
GO_VERSION = "go1.27.3"
PROTOBUF_VERSION = "v1.36.12"
GRPC_PLUGIN_VERSION = "v1.5.1"


def run(*args: str, env: dict[str, str] | None = None) -> None:
    subprocess.run(args, cwd=ROOT, env=env, check=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail on generated source drift")
    args = parser.parse_args()
    version = subprocess.check_output(["go", "version"], text=True).split()[2]
    if version != GO_VERSION:
        raise SystemExit(f"codegen requires {GO_VERSION}; found {version}")
    cache = Path(os.environ.get("HYPERCALL_TOOL_CACHE", Path.home() / ".cache/hypercall/codegen"))
    plugins = cache / f"{GO_VERSION}-protobuf{PROTOBUF_VERSION[1:]}-grpc{GRPC_PLUGIN_VERSION[1:]}"
    plugins.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOBIN=str(plugins))
    for binary, module, version in (
        ("protoc-gen-go", "google.golang.org/protobuf/cmd/protoc-gen-go", PROTOBUF_VERSION),
        ("protoc-gen-go-grpc", "google.golang.org/grpc/cmd/protoc-gen-go-grpc", GRPC_PLUGIN_VERSION),
    ):
        if not (plugins / binary).is_file():
            run("go", "install", f"{module}@{version}", env=env)
    with tempfile.TemporaryDirectory(prefix="hypercall-codegen-") as tmp:
        output = Path(tmp)
        go_out, py_out = output / "gen/go", output / "python/hypercall_proto"
        go_out.mkdir(parents=True)
        py_out.mkdir(parents=True)
        run(sys.executable, "-m", "grpc_tools.protoc", "-Iproto",
            f"--plugin=protoc-gen-go={plugins / 'protoc-gen-go'}",
            f"--plugin=protoc-gen-go-grpc={plugins / 'protoc-gen-go-grpc'}",
            f"--go_out={go_out}", "--go_opt=paths=source_relative",
            f"--go-grpc_out={go_out}", "--go-grpc_opt=paths=source_relative",
            f"--python_out={py_out}", f"--pyi_out={py_out}", f"--grpc_python_out={py_out}",
            "hypercall/guest/v1/guest.proto")
        # protoc's Python package follows proto paths; the distribution adds an
        # explicit hypercall_proto namespace to avoid collisions with a later SDK.
        grpc_file = py_out / "hypercall/guest/v1/guest_pb2_grpc.py"
        grpc_file.write_text(grpc_file.read_text().replace(
            "from hypercall.guest.v1 import", "from hypercall_proto.hypercall.guest.v1 import"))
        for directory in (py_out / "hypercall", py_out / "hypercall/guest", py_out / "hypercall/guest/v1"):
            (directory / "__init__.py").write_text('"""Generated protocol namespace."""\n')
        expected = {p.relative_to(output) for p in output.rglob("*") if p.is_file()}
        actual = {p.relative_to(ROOT) for base in (ROOT / "gen/go", ROOT / "python/hypercall_proto/hypercall")
                  for p in base.rglob("*") if p.is_file() and "__pycache__" not in p.parts}
        if args.check:
            changed = sorted(str(path) for path in expected | actual
                             if path not in expected or path not in actual
                             or not filecmp.cmp(output / path, ROOT / path, shallow=False))
            if changed:
                raise SystemExit("generated source drift:\n" + "\n".join(changed))
            print("generated sources are current")
        else:
            for path in actual - expected:
                (ROOT / path).unlink()
            for path in expected:
                (ROOT / path).parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(output / path, ROOT / path)


if __name__ == "__main__":
    main()
