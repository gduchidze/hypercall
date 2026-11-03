"""Operator-command safety checks; these never contact a host or database."""
import json
import contextlib
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import io
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import runpy
import socket
import threading
import unittest
from unittest.mock import patch
from hypercall import types


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts/qualify-phase2a"


def available_address():
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return f"127.0.0.1:{listener.getsockname()[1]}"


class ExitedProcess:
    def poll(self):
        return 1

    def terminate(self):
        pass

    def wait(self, timeout):
        return 1


class RuntimeQualificationRefusal(unittest.TestCase):
    def test_published_native_metadata_accepts_verified_firecracker_version_format(self):
        command = runpy.run_path(str(SCRIPT))
        task = types.TaskSpec(template_digest="sha256:" + "a" * 64, agent_digest="sha256:" + "b" * 64,
                              limits=types.Limits(cpu=1, memory_mb=256, disk_mb=8))
        # Sanitized metadata contract: the version string is the verified
        # artifact-config value observed on the real host. No VM bytes or runtime
        # evidence are fabricated by this metadata-only regression fixture.
        native = {"schema_version": 1, "scope": {"tenant_id": "phase2a", "project_id": "runtime-native",
                  "checkpoint_id": "cp-sanitized"}, "compatibility": {
                  "architecture": "aarch64", "firecracker_version": "Firecracker v1.14.0",
                  "snapshot_format_version": "8.0.0", "barrier_protocol_version": "controlled-python-v2-host-clock",
                  "memory_mib": 256, "vcpus": 1, "template_sha256": "a" * 64, "guest_sha256": "b" * 64},
                  "artifacts": [{"kind": kind, "size": size, "sha256": "c" * 64,
                                 "key": "tenants/phase2a/projects/runtime-native/checkpoints/cp-sanitized/upload/" + kind}
                                for kind, size in (("memory", 256 << 20), ("vmstate", 1024), ("disk", 8 << 20))]}
        encoded = json.dumps(native, separators=(",", ":")).encode()
        spec = types.CheckpointSpec(verified=True, branch_id="retained-branch", native_checkpoint_id="cp-sanitized",
                                    protocol_version="hypercall.snapshot.v1", snapshot_manifest=encoded,
                                    manifest_digest="sha256:" + hashlib.sha256(encoded).hexdigest())
        workflow = command["Workflow"].__new__(command["Workflow"])
        workflow.config = {"tenant_id": "phase2a", "project_id": "runtime-native", "architecture": "aarch64"}
        workflow.branch_id, workflow.report = "retained-branch", {}
        workflow.checkpoint(types.Resource(kind="Checkpoint", checkpoint_spec=spec), task)
        self.assertEqual(workflow.report["native_snapshot"], {
            "manifest_digest": spec.manifest_digest, "memory_bytes": 256 << 20, "architecture": "aarch64"})
        # A valid display version must not permit a tiny state-conformance
        # artifact to satisfy the full-RAM native workflow gate.
        native["artifacts"][0]["size"] = 4
        spec.snapshot_manifest = json.dumps(native, separators=(",", ":")).encode()
        spec.manifest_digest = "sha256:" + hashlib.sha256(spec.snapshot_manifest).hexdigest()
        with self.assertRaisesRegex(command["QualificationFailure"], "full_native_snapshot_required"):
            workflow.checkpoint(types.Resource(kind="Checkpoint", checkpoint_spec=spec), task)

    def test_missing_opt_in_refuses_before_reading_inputs_or_creating_report(self):
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary) / "report.json"
            environment = dict(os.environ)
            environment.pop("HYPERCALL_RUNTIME_2A_TEST", None)
            result = subprocess.run([sys.executable, str(SCRIPT), "--config", "/missing/config.json",
                                     "--report", str(report)], env=environment, capture_output=True, text=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("HYPERCALL_RUNTIME_2A_TEST=1_required", result.stderr)
            self.assertFalse(report.exists())

    def test_dead_api_with_known_branch_reports_unconfirmed_cleanup(self):
        command = runpy.run_path(str(SCRIPT))
        report = {"status": "failed"}
        workflow = command["Workflow"]({"grpc_address": available_address(), "project_id": "test"},
                                       "test-only-key-01234567890123456789", report)
        workflow.branch_id = "retained-branch"
        workflow.process = ExitedProcess()
        workflow.close()
        self.assertIn("unconfirmed", report["cleanup"])
        self.assertIn("reconcile execution lease", report["cleanup"])

    def test_operator_report_survives_process_shutdown_error(self):
        command = runpy.run_path(str(SCRIPT))

        class ShutdownFailure(ExitedProcess):
            def terminate(self):
                raise OSError("sensitive process diagnostic must not enter the report")

        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            binary = directory / "not-a-runtime-test-process"
            binary.write_bytes(b"#!/bin/sh\nexit 1\n")
            binary.chmod(0o700)
            secret = directory / "private-config"
            secret.write_text("not-contacted")
            secret.chmod(0o600)
            manifest = directory / "task.yaml"
            manifest.write_text("kind: Task\nspec:\n  execution_mode: controlled_python_v1\n  limits:\n    timeout_seconds: 600\n")
            config = {"profile": "controlled_python_v1_firecracker", "architecture": "aarch64",
                      "server_binary": str(binary), "cli_binary": str(binary),
                      "server_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                      "cli_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                      "keys_file": str(secret), "host_key": str(secret), "host_ca": str(secret),
                      "host_cert": str(secret), "manifest_file": str(manifest),
                      "tenant_id": "tenant", "project_id": "project", "host_server_name": "not-contacted",
                      "grpc_address": available_address(), "http_address": available_address(),
                      "host_address": "127.0.0.1:9444"}
            source = directory / "operator.json"
            source.write_text(json.dumps(config))
            source.chmod(0o600)
            output = directory / "report.json"
            environment = {"HYPERCALL_RUNTIME_2A_TEST": "1", "HYPERCALL_DATABASE_URL": "postgres://not-contacted/test",
                           "HYPERCALL_API_KEY": "test-only-key-01234567890123456789"}
            # The process is the external boundary: no server, DB or VM starts.
            with patch.dict(os.environ, environment), patch.object(command["subprocess"], "Popen", return_value=ShutdownFailure()), \
                    patch.object(sys, "argv", [str(SCRIPT), "--config", str(source), "--report", str(output)]), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(command["main"](), 1)
            report = json.loads(output.read_text())
            self.assertEqual(report["status"], "failed")
            self.assertEqual(report["failure"], "server_start_failed")
            self.assertEqual(report["cleanup_failure"], "OSError")
            self.assertNotIn("sensitive process diagnostic", output.read_text())

    def test_loopback_rest_bypasses_environment_proxy(self):
        command = runpy.run_path(str(SCRIPT))
        received = []
        proxied = []

        class Endpoint(BaseHTTPRequestHandler):
            def do_GET(self):
                received.append((self.path, self.headers.get("Authorization")))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"metadata":{"id":"branch"}}')

            def log_message(self, *_):
                pass

        class Proxy(Endpoint):
            def do_GET(self):
                proxied.append(self.headers.get("Authorization"))
                self.send_response(502)
                self.end_headers()

        endpoint, proxy = HTTPServer(("127.0.0.1", 0), Endpoint), HTTPServer(("127.0.0.1", 0), Proxy)
        for server in (endpoint, proxy):
            threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            proxy_url = f"http://127.0.0.1:{proxy.server_port}"
            with patch.dict(os.environ, {"HTTP_PROXY": proxy_url, "http_proxy": proxy_url, "NO_PROXY": "", "no_proxy": ""}):
                resource = command["inspect_rest"](f"127.0.0.1:{endpoint.server_port}", "project", "branch", "local-test-key")
            self.assertEqual(resource["metadata"]["id"], "branch")
            self.assertEqual(received, [("/v1/projects/project/resources/Branch/branch", "Bearer local-test-key")])
            self.assertEqual(proxied, [])
        finally:
            for server in (endpoint, proxy):
                server.shutdown()
                server.server_close()

    def test_conformance_profile_cannot_run_as_real_runtime_qualification(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            config = {key: "not-used" for key in (
                "profile", "server_binary", "server_sha256", "cli_binary", "cli_sha256", "keys_file",
                "manifest_file", "tenant_id", "project_id", "grpc_address", "http_address", "host_address",
                "host_ca", "host_cert", "host_key", "host_server_name", "architecture")}
            config["profile"] = "test-only-state-v1"
            source = directory / "config.json"
            source.write_text(json.dumps(config))
            source.chmod(0o600)
            report = directory / "report.json"
            key = "test-only-secret-01234567890123456789012345"
            database = "postgres://must-never-be-contacted/qualification"
            environment = dict(os.environ, HYPERCALL_RUNTIME_2A_TEST="1", HYPERCALL_API_KEY=key,
                               HYPERCALL_DATABASE_URL=database)
            result = subprocess.run([sys.executable, str(SCRIPT), "--config", str(source),
                                     "--report", str(report)], env=environment, capture_output=True, text=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("real_profile_required", result.stderr)
            self.assertNotIn(key, result.stderr + result.stdout)
            self.assertNotIn(database, result.stderr + result.stdout)
            self.assertFalse(report.exists())


if __name__ == "__main__":
    unittest.main()
