"""Exercise the actual Go guest through its private test-only Unix transport."""
from __future__ import annotations

import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
import subprocess
import tempfile
import unittest

import grpc
from hypercall_proto.hypercall.guest.v1 import guest_pb2 as pb
from hypercall_proto.hypercall.guest.v1 import guest_pb2_grpc as rpc

ROOT = Path(__file__).resolve().parents[2]


class GuestContract(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="hypercall-contract-", dir="/tmp")
        cls.addClassCleanup(cls.temp.cleanup)
        cls.directory = Path(cls.temp.name)
        cls.binary = cls.directory / "guest"
        subprocess.run(["go", "build", "-o", str(cls.binary), "./cmd/hypercall-guest"], cwd=ROOT, check=True)
        cls.workspace = cls.directory / "workspace"
        cls.workspace.mkdir()
        cls.socket = cls.directory / "guest.sock"
        cls.session = cls.directory / "session.json"
        cls.session.write_text(json.dumps({
            "identity": {"tenant_id": "tenant-contract", "project_id": "project-contract",
                         "sandbox_id": "sandbox-contract", "execution_id": "execution-contract", "epoch": 7},
            "credential": "test-only-contract-session-credential",
            "expires_at": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat().replace("+00:00", "Z"),
        }))
        cls.session.chmod(0o600)
        cls.process = subprocess.Popen([str(cls.binary), "--test-unix-socket", str(cls.socket),
                                        "--workspace", str(cls.workspace), "--session-config", str(cls.session)],
                                       cwd=ROOT, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                                       env=dict(os.environ, HYPERCALL_CONTRACT_HOST_ONLY="synthetic-host-value"))
        cls.addClassCleanup(cls.stop_guest)
        cls.channel = grpc.insecure_channel("unix:" + str(cls.socket))
        cls.addClassCleanup(cls.channel.close)
        grpc.channel_ready_future(cls.channel).result(timeout=10)
        cls.client = rpc.GuestStub(cls.channel)
        cls.metadata = (("authorization", "Bearer test-only-contract-session-credential"),)

    @classmethod
    def stop_guest(cls):
        cls.process.terminate()
        try:
            cls.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            cls.process.kill()
            cls.process.wait(timeout=5)

    def test_allocation_identity_comes_from_authenticated_session(self):
        info = self.client.GetInfo(pb.GetInfoRequest(), metadata=self.metadata, timeout=3)
        self.assertEqual(info, pb.GetInfoResponse(
            protocol_version="hypercall.guest.v1",
            identity=pb.AllocationIdentity(tenant_id="tenant-contract", project_id="project-contract",
                                          sandbox_id="sandbox-contract", execution_id="execution-contract", epoch=7),
            max_file_bytes=1048576, max_exec_output_bytes=1048576))

    def test_binary_file_roundtrip_preserves_non_utf8_and_nul(self):
        payload = b"\x00\xff\x80binary\n\x00"
        written = self.client.WriteFile(pb.WriteFileRequest(path="binary.dat", data=payload),
                                        metadata=self.metadata, timeout=3)
        read = self.client.ReadFile(pb.ReadFileRequest(path="binary.dat"), metadata=self.metadata, timeout=3)
        self.assertEqual((written.bytes_written, read.data), (11, payload))

    def test_exec_preserves_output_and_one_terminal_nonzero_result(self):
        events = list(self.client.Exec(pb.ExecRequest(
            argv=["/bin/sh", "-c", "printf 'out'; printf 'err' >&2; exit 7"], timeout_ms=3000),
            metadata=self.metadata, timeout=5))
        stdout = b"".join(event.stdout for event in events if event.WhichOneof("event") == "stdout")
        stderr = b"".join(event.stderr for event in events if event.WhichOneof("event") == "stderr")
        results = [event.result for event in events if event.WhichOneof("event") == "result"]
        self.assertEqual((stdout, stderr, results, events[-1].WhichOneof("event")),
                         (b"out", b"err", [pb.ExecResult(exit_code=7)], "result"))

    def test_missing_wrong_or_duplicate_session_is_unauthenticated(self):
        for metadata in ((), (("authorization", "Bearer wrong"),), self.metadata + self.metadata):
            with self.subTest(metadata_count=len(metadata)):
                with self.assertRaises(grpc.RpcError) as failure:
                    self.client.GetInfo(pb.GetInfoRequest(), metadata=metadata, timeout=3)
                self.assertEqual(failure.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.RpcError) as failure:
            list(self.client.Exec(pb.ExecRequest(argv=["/bin/echo", "denied"], timeout_ms=1000), timeout=3))
        self.assertEqual(failure.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    def test_file_bounds_and_traversal_keep_canonical_errors(self):
        for request, expected in (
            (pb.WriteFileRequest(path="../escape", data=b"x"), grpc.StatusCode.INVALID_ARGUMENT),
            (pb.WriteFileRequest(path="oversize", data=b"x" * 1048577), grpc.StatusCode.RESOURCE_EXHAUSTED),
        ):
            with self.subTest(code=expected):
                with self.assertRaises(grpc.RpcError) as failure:
                    self.client.WriteFile(request, metadata=self.metadata, timeout=3)
                self.assertEqual(failure.exception.code(), expected)
        with self.assertRaises(grpc.RpcError) as failure:
            self.client.ReadFile(pb.ReadFileRequest(path="missing"), metadata=self.metadata, timeout=3)
        self.assertEqual(failure.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_rpc_cancellation_ends_the_foreground_process_group(self):
        call = self.client.Exec(pb.ExecRequest(argv=["/bin/sh", "-c",
            "(/bin/sleep 1; printf leaked > cancellation-leak) & printf ready; wait"], timeout_ms=5000),
            metadata=self.metadata, timeout=7)
        self.addCleanup(call.cancel)
        self.assertEqual(next(call).stdout, b"ready")
        self.assertTrue(call.cancel())
        with self.assertRaises(grpc.RpcError) as failure:
            next(call)
        self.assertEqual(failure.exception.code(), grpc.StatusCode.CANCELLED)
        # Observe only the RPC seam after the descendant would have written.
        list(self.client.Exec(pb.ExecRequest(argv=["/bin/sleep", "1.5"], timeout_ms=3000),
                              metadata=self.metadata, timeout=5))
        with self.assertRaises(grpc.RpcError) as failure:
            self.client.ReadFile(pb.ReadFileRequest(path="cancellation-leak"), metadata=self.metadata, timeout=3)
        self.assertEqual(failure.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_exec_environment_is_minimal_with_allowlisted_overrides(self):
        events = list(self.client.Exec(pb.ExecRequest(argv=["/usr/bin/env"], env={"LANG": "contract"}, timeout_ms=1000),
                                       metadata=self.metadata, timeout=3))
        output = b"".join(event.stdout for event in events if event.WhichOneof("event") == "stdout")
        self.assertEqual(dict(line.split(b"=", 1) for line in output.splitlines()),
                         {b"PATH": b"/usr/local/bin:/usr/bin:/bin", b"LANG": b"contract"})

    def test_command_timeout_is_a_result_and_rpc_deadline_is_an_error(self):
        events = list(self.client.Exec(pb.ExecRequest(argv=["/bin/sleep", "5"], timeout_ms=100),
                                       metadata=self.metadata, timeout=3))
        self.assertEqual((len(events), events[-1].WhichOneof("event"), events[-1].result.timed_out),
                         (1, "result", True))
        with self.assertRaises(grpc.RpcError) as failure:
            list(self.client.Exec(pb.ExecRequest(argv=["/bin/sleep", "5"], timeout_ms=3000),
                                  metadata=self.metadata, timeout=0.1))
        self.assertEqual(failure.exception.code(), grpc.StatusCode.DEADLINE_EXCEEDED)


if __name__ == "__main__":
    unittest.main()
