"""Sequential cooperative agent hooks over the guest-local Unix gRPC seam."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import re
import random
import threading

import grpc
from hypercall_proto.hypercall.harness.v1 import harness_pb2 as pb
from hypercall_proto.hypercall.harness.v1.harness_pb2_grpc import HarnessStub

from .client import HypercallError

PROTOCOL = "hypercall.harness.v1"
_IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z")
_DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def digest(data: bytes) -> str:
    return "sha256:" + hashlib.sha256(data).hexdigest()


def validate_session(session: pb.HarnessSession) -> None:
    """Verify the controller-sealed cross-language session before using it."""
    if (session.protocol_version != PROTOCOL or session.epoch < 1
            or not _DIGEST.fullmatch(session.route_digest) or not _DIGEST.fullmatch(session.model_digest)
            or any(not _IDENTIFIER.fullmatch(getattr(session, name)) for name in
                   ("tenant_id", "project_id", "branch_id", "sandbox_id", "execution_id", "call_namespace"))):
        raise HypercallError("failed_precondition", "invalid cooperative session")
    if bool(session.parent_checkpoint_id) != session.HasField("inherited_barrier"):
        raise HypercallError("failed_precondition", "inherited checkpoint and barrier must be paired")
    if session.HasField("inherited_barrier"):
        marker = session.inherited_barrier
        if (not _IDENTIFIER.fullmatch(session.parent_checkpoint_id) or marker.protocol_version != PROTOCOL
                or marker.step < 0 or marker.event_sequence < 1 or marker.source_epoch < 1
                or not _IDENTIFIER.fullmatch(marker.source_branch_id)
                or not _IDENTIFIER.fullmatch(marker.source_execution_id)
                or not _DIGEST.fullmatch(marker.state_digest) or not _DIGEST.fullmatch(marker.source_session_hash)):
            raise HypercallError("failed_precondition", "invalid inherited cooperative barrier")
    unsigned = pb.HarnessSession()
    unsigned.CopyFrom(session)
    unsigned.ClearField("session_hash")
    if session.session_hash != digest(unsigned.SerializeToString(deterministic=True)):
        raise HypercallError("failed_precondition", "cooperative session hash mismatch")


class AgentActionError(HypercallError):
    def __init__(self, result: pb.AgentResult):
        super().__init__("failed_precondition", "registered action did not complete: " + result.state,
                         ambiguous=result.state == "unknown")
        self.result = result


class ModelClient:
    """A session-bound model client. A restored parent client cannot make calls."""
    def __init__(self, agent: AgentHarness):
        self._agent, self._session_hash = agent, agent.session.session_hash

    def generate(self, payload: bytes, *, step: int, position: int) -> pb.AgentResult:
        return self._agent.call("inference", "inference", payload, step=step, position=position,
                                expected_session_hash=self._session_hash)


class AgentHarness:
    """One foreground agent with explicit step commits and resume hooks.

    Background and overlapping actions are unsupported. Calls are never retried
    implicitly. Ingress keys/provider credentials do not enter this interface.
    """
    def __init__(self, socket_path: str, *, timeout: float = 30):
        if not Path(socket_path).is_absolute() or timeout <= 0:
            raise ValueError("absolute guest-local socket and positive timeout required")
        self._target, self._timeout = "unix:" + socket_path, timeout
        self._lock = threading.RLock()
        self._owner = threading.get_ident()
        self._session = pb.HarnessSession()
        self._closed, self._barrier = False, None
        self._connect()
        try:
            self._session.CopyFrom(self._invoke("GetSession", pb.GetSessionRequest()))
            validate_session(self._session)
            self._acknowledge(timeout)
        except BaseException:
            self.close()
            raise

    def _connect(self):
        self._channel = grpc.insecure_channel(self._target, options=(("grpc.enable_retries", 0),))
        self._stub = HarnessStub(self._channel)

    def _invoke(self, method, request, *, timeout=None):
        try:
            return getattr(self._stub, method)(request, timeout=self._timeout if timeout is None else timeout)
        except grpc.RpcError as error:
            raise HypercallError.from_rpc(error, mutation=method != "GetSession") from error

    def _acknowledge(self, timeout):
        # Native restore renews the kernel CSPRNG first. Reset the supported
        # Python module RNG before acknowledging any resumed action authority.
        # Independent Random instances and third-party RNGs are unsupported.
        random.seed(os.urandom(32))
        ack = self._invoke("AcknowledgeResume", pb.SessionAckRequest(session_hash=self._session.session_hash), timeout=timeout)
        if ack.session_hash != self._session.session_hash:
            raise HypercallError("failed_precondition", "controller acknowledged a different session")

    @property
    def session(self) -> pb.HarnessSession:
        with self._lock:
            copy = pb.HarnessSession()
            copy.CopyFrom(self._session)
            return copy

    def _foreground(self):
        if threading.get_ident() != self._owner:
            raise HypercallError("failed_precondition", "background cooperative actions are unsupported")

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def close(self):
        with self._lock:
            if not self._closed:
                self._closed = True
                self._channel.close()

    def model_client(self) -> ModelClient:
        return ModelClient(self)

    def call(self, kind: str, name: str, payload: bytes, *, step: int, position: int,
             expected_session_hash: str | None = None) -> pb.AgentResult:
        self._foreground()
        with self._lock:
            if self._closed or self._barrier is not None:
                raise HypercallError("failed_precondition", "cooperative action gate is closed")
            if expected_session_hash is not None and expected_session_hash != self._session.session_hash:
                raise HypercallError("failed_precondition", "cached model client belongs to an inherited session")
            if not isinstance(payload, bytes) or len(payload) > 64 << 10 or not 1 <= step <= 100000 or not 1 <= position <= 1024:
                raise ValueError("bounded bytes payload, step and position required")
            request = pb.AgentRequest(id=f"{self._session.call_namespace}/{step}/{position}/{kind}",
                                      kind=kind, step=step, position=position, name=name, payload=payload,
                                      payload_digest=digest(payload), session_hash=self._session.session_hash)
            result = self._invoke("Submit", request)
            if result.id != request.id or result.event_sequence < 1:
                raise HypercallError("data_loss", "registered action result identity/cursor mismatch", ambiguous=True)
            expected_state = {"inference": "completed", "tool": "completed", "step_commit": "committed", "finalize": "finalized"}.get(kind)
            if result.state != expected_state or result.error_code:
                raise AgentActionError(result)
            return result

    def tool(self, name: str, payload: bytes, *, step: int, position: int) -> pb.AgentResult:
        return self.call("tool", name, payload, step=step, position=position)

    def commit_step(self, step: int, state: bytes, *, position: int) -> pb.AgentBarrier:
        result = self.call("step_commit", "", state, step=step, position=position)
        if result.payload != digest(state).encode():
            raise HypercallError("data_loss", "committed state digest differs from registered input")
        return pb.AgentBarrier(step=step, event_sequence=result.event_sequence, state_digest=digest(state),
                               protocol_version=PROTOCOL, session_hash=self._session.session_hash)

    def finalize(self, step: int, output: bytes, *, position: int) -> pb.AgentResult:
        return self.call("finalize", "", output, step=step, position=position)

    def await_resume(self, barrier: pb.AgentBarrier, *, timeout: float = 3600) -> pb.HarnessSession:
        self._foreground()
        with self._lock:
            if (self._closed or barrier.session_hash != self._session.session_hash
                    or barrier.protocol_version != PROTOCOL or barrier.event_sequence < 1
                    or not _DIGEST.fullmatch(barrier.state_digest) or timeout <= 0):
                raise HypercallError("failed_precondition", "barrier does not match the current cooperative session")
            self._barrier = pb.AgentBarrier()
            self._barrier.CopyFrom(barrier)
            previous = self.session
            refreshed = self._invoke("AwaitResume", barrier, timeout=timeout)
            validate_session(refreshed)
            marker = refreshed.inherited_barrier
            if (not refreshed.HasField("inherited_barrier") or refreshed.session_hash == previous.session_hash
                    or refreshed.execution_id == previous.execution_id or refreshed.sandbox_id == previous.sandbox_id
                    or refreshed.call_namespace == previous.call_namespace or marker.source_session_hash != previous.session_hash
                    or marker.source_branch_id != previous.branch_id or marker.source_execution_id != previous.execution_id
                    or marker.source_epoch != previous.epoch or marker.step != barrier.step
                    # Sealing appends a durable barrier event after step commit.
                    # Guest/native restore verifies that exact final marker;
                    # this hook additionally requires it to cover our commit.
                    or marker.event_sequence < barrier.event_sequence or marker.state_digest != barrier.state_digest):
                raise HypercallError("failed_precondition", "restored configuration differs from the retained semantic barrier")
            # Drop inherited transports and cached model clients before local ACK.
            self._channel.close()
            self._connect()
            observed = self._invoke("GetSession", pb.GetSessionRequest())
            if observed != refreshed:
                raise HypercallError("failed_precondition", "resume configuration changed during client refresh")
            self._session.CopyFrom(refreshed)
            self._acknowledge(timeout)
            self._barrier = None
            return self.session
