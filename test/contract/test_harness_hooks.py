"""Python hook behavior against a guest-local RPC conformance adapter; no VMs."""
from concurrent.futures import ThreadPoolExecutor
import tempfile
import random
import threading
from pathlib import Path
import unittest

import grpc
from hypercall import HypercallError
from hypercall.harness import AgentHarness, digest
from hypercall_proto.hypercall.harness.v1 import harness_pb2 as pb
from hypercall_proto.hypercall.harness.v1 import harness_pb2_grpc as rpc


def sealed(**fields):
    session = pb.HarnessSession(**fields)
    session.session_hash = digest(session.SerializeToString(deterministic=True))
    return session


class GuestRPCConformance(rpc.HarnessServicer):
    def __init__(self):
        self.parent = sealed(tenant_id="tenant", project_id="project", branch_id="parent",
                             sandbox_id="parent-vm", execution_id="parent-execution", epoch=1,
                             call_namespace="parent-calls", route_digest=digest(b"parent-route"),
                             model_digest=digest(b"parent-model"), protocol_version="hypercall.harness.v1")
        self.current, self.requests = self.parent, []
        self.block_resume = False
        self.entered, self.release = threading.Event(), threading.Event()
        self.tampered = False

    def GetSession(self, request, context):
        copy = pb.HarnessSession()
        copy.CopyFrom(self.current)
        if self.tampered:
            copy.route_digest = digest(b"unacknowledged-route")
        return copy

    def AcknowledgeResume(self, request, context):
        if request.session_hash != self.current.session_hash:
            context.abort(grpc.StatusCode.FAILED_PRECONDITION, "wrong session")
        return pb.SessionAck(session_hash=request.session_hash)

    def Submit(self, request, context):
        self.requests.append(request)
        if request.session_hash != self.current.session_hash:
            context.abort(grpc.StatusCode.FAILED_PRECONDITION, "stale session")
        state = {"step_commit": "committed", "finalize": "finalized"}.get(request.kind, "completed")
        payload = request.payload
        if request.kind == "step_commit":
            payload = digest(payload).encode()
        elif request.kind == "inference":
            payload = b"child-routed-output"
        return pb.AgentResult(id=request.id, state=state, payload=payload,
                               event_sequence=5 if request.kind == "step_commit" else 9)

    def AwaitResume(self, request, context):
        self.entered.set()
        if self.block_resume and not self.release.wait(2):
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "background action waited across barrier")
        marker = pb.SemanticBarrier(protocol_version="hypercall.harness.v1", step=request.step,
                                    event_sequence=request.event_sequence + 3, state_digest=request.state_digest,
                                    source_session_hash=self.parent.session_hash, source_branch_id=self.parent.branch_id,
                                    source_execution_id=self.parent.execution_id, source_epoch=self.parent.epoch)
        self.current = sealed(tenant_id="tenant", project_id="project", branch_id="child",
                              sandbox_id="child-vm", execution_id="child-execution", epoch=1,
                              call_namespace="child-calls", route_digest=digest(b"child-route"),
                              model_digest=digest(b"child-model"), protocol_version="hypercall.harness.v1",
                              parent_checkpoint_id="retained-checkpoint", inherited_barrier=marker)
        return self.current


class PythonResumeHooks(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="hc-hook-")
        self.socket = str(Path(self.directory.name) / "harness.sock")
        self.adapter = GuestRPCConformance()
        self.server = grpc.server(ThreadPoolExecutor(max_workers=4))
        rpc.add_HarnessServicer_to_server(self.adapter, self.server)
        self.server.add_insecure_port("unix:" + self.socket)
        self.server.start()

    def tearDown(self):
        self.adapter.release.set()
        self.server.stop(0).wait()
        self.directory.cleanup()

    def test_resume_covers_advanced_seal_cursor_and_invalidates_cached_model_client(self):
        with AgentHarness(self.socket, timeout=3) as agent:
            inherited_client = agent.model_client()
            barrier = agent.commit_step(1, b'{"prefix":"committed"}', position=1)
            random.seed(123)
            inherited_random_state = random.getstate()
            child = agent.await_resume(barrier, timeout=3)
            self.assertTrue(random.getstate() != inherited_random_state, "Python module RNG was inherited across resume")
            self.assertEqual(child.inherited_barrier.event_sequence, 8)
            before = len(self.adapter.requests)
            with self.assertRaises(HypercallError) as failure:
                inherited_client.generate(b"suffix", step=2, position=1)
            self.assertEqual(failure.exception.code, "failed_precondition")
            self.assertEqual(len(self.adapter.requests), before)
            result = agent.model_client().generate(b"suffix", step=2, position=1)
            self.assertEqual(result.payload, b"child-routed-output")
            self.assertEqual(self.adapter.requests[-1].session_hash, child.session_hash)
            self.assertTrue(self.adapter.requests[-1].id.startswith("child-calls/"))

    def test_background_action_is_rejected_while_foreground_waits_at_barrier(self):
        self.adapter.block_resume = True
        errors = []
        with AgentHarness(self.socket, timeout=3) as agent:
            barrier = agent.commit_step(1, b"state", position=1)

            def background():
                self.adapter.entered.wait(1)
                try:
                    agent.tool("echo", b"unregistered", step=2, position=1)
                except HypercallError as error:
                    errors.append(error.code)
                finally:
                    self.adapter.release.set()

            thread = threading.Thread(target=background, daemon=True)
            thread.start()
            agent.await_resume(barrier, timeout=3)
            thread.join(timeout=1)
            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, ["failed_precondition"])
            self.assertEqual([request.kind for request in self.adapter.requests], ["step_commit"])

    def test_unsealed_routing_configuration_fails_before_any_action(self):
        self.adapter.tampered = True
        with self.assertRaises(HypercallError) as failure:
            AgentHarness(self.socket, timeout=3)
        self.assertEqual(failure.exception.code, "failed_precondition")
        self.assertEqual(self.adapter.requests, [])
