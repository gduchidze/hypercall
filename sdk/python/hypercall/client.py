from __future__ import annotations

from dataclasses import dataclass
import ipaddress
from typing import AsyncIterator, Iterator
from urllib.parse import urlsplit

import grpc
from hypercall_proto.hypercall.guest.v1 import guest_pb2 as guest
from hypercall_proto.hypercall.v1 import control_pb2 as pb
from hypercall_proto.hypercall.v1.control_pb2_grpc import ControlPlaneStub


class HypercallError(Exception):
    """Canonical gRPC status. The SDK never automatically retries operations.

    An ambiguous mutation must be reconciled using its original idempotency key;
    commands must not be replayed automatically after an ambiguous outcome.
    """
    def __init__(self, code: str, message: str, *, retryable: bool = False,
                 ambiguous: bool = False, operation_id: str | None = None):
        super().__init__(message)
        self.code, self.retryable = code, retryable
        self.ambiguous, self.operation_id = ambiguous, operation_id

    @classmethod
    def from_rpc(cls, error: grpc.RpcError, *, mutation: bool = False) -> HypercallError:
        code = error.code().name.lower()
        metadata = dict(error.trailing_metadata() or ())
        return cls(code, error.details() or code,
                   retryable=code in {"unavailable", "resource_exhausted", "aborted"},
                   ambiguous=mutation and code in {"unavailable", "deadline_exceeded", "cancelled", "unknown"},
                   operation_id=metadata.get("operation-id"))


@dataclass(frozen=True)
class CommandResult:
    stdout: bytes
    stderr: bytes
    exit_code: int
    timed_out: bool
    output_truncated: bool


def _validate(endpoint: str, project: str, api_key: str, timeout: float, plaintext: bool) -> None:
    if not endpoint or not project or not api_key or timeout <= 0:
        raise ValueError("endpoint, project, api_key and a positive timeout are required")
    parsed = urlsplit("//" + endpoint)
    if not parsed.hostname or not parsed.port or parsed.path or parsed.username:
        raise ValueError("endpoint must be host:port")
    if plaintext and parsed.hostname != "localhost":
        try:
            loopback = ipaddress.ip_address(parsed.hostname).is_loopback
        except ValueError:
            loopback = False
        if not loopback:
            raise ValueError("plaintext is limited to loopback endpoints")


class _Configuration:
    def __init__(self, endpoint: str, *, project: str, api_key: str,
                 timeout: float = 30, plaintext: bool = False,
                 root_certificates: bytes | None = None):
        _validate(endpoint, project, api_key, timeout, plaintext)
        self.project, self.timeout = project, timeout
        self._metadata = (("authorization", "Bearer " + api_key),)
        self._endpoint, self._plaintext, self._roots = endpoint, plaintext, root_certificates

    def _kwargs(self, timeout: float | None) -> dict:
        value = self.timeout if timeout is None else timeout
        if value <= 0:
            raise ValueError("positive deadline required")
        return {"metadata": self._metadata, "timeout": value}

    def _request(self, message, **values):
        return message(project_id=self.project, **values)


class Client(_Configuration):
    def compile_evaluation(self, config: str, *, timeout: float | None = None) -> pb.EvaluationPlan:
        return self._call("CompileEvaluation", self._request(pb.CompileEvaluationRequest, config_yaml=config), timeout=timeout)

    def evaluate(self, config: str, observations: list[pb.EvaluationObservation], *, idempotency_key: str,
                 timeout: float | None = None) -> pb.Evaluation:
        return self._call("SubmitEvaluation", self._request(pb.SubmitEvaluationRequest, config_yaml=config,
                          observations=observations, idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def evaluation(self, evaluation_id: str, *, timeout: float | None = None) -> pb.Evaluation:
        return self._call("GetEvaluation", self._request(pb.GetEvaluationRequest, id=evaluation_id), timeout=timeout)

    def report(self, evaluation_id: str, *, timeout: float | None = None) -> pb.EvaluationReport:
        return self._call("GetReport", self._request(pb.GetReportRequest, id=evaluation_id), timeout=timeout)

    def projection_status(self, *, timeout: float | None = None) -> pb.ProjectionStatusResponse:
        return self._call("ProjectionStatus", self._request(pb.ProjectionStatusRequest), timeout=timeout)

    def __init__(self, endpoint: str, **kwargs):
        super().__init__(endpoint, **kwargs)
        options = (("grpc.enable_retries", 0), ("grpc.max_receive_message_length", 4 << 20))
        self._channel = (grpc.insecure_channel(endpoint, options=options) if self._plaintext
                         else grpc.secure_channel(endpoint, grpc.ssl_channel_credentials(self._roots), options=options))
        self._stub = ControlPlaneStub(self._channel)

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()

    def close(self) -> None:
        self._channel.close()

    def _call(self, method: str, request, *, timeout: float | None = None, mutation: bool = False):
        try:
            return getattr(self._stub, method)(request, **self._kwargs(timeout))
        except grpc.RpcError as error:
            raise HypercallError.from_rpc(error, mutation=mutation) from error

    def submit_yaml(self, manifest: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Submission:
        return self._call("SubmitManifest", self._request(pb.SubmitManifestRequest, manifest_yaml=manifest,
                          idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def submit(self, task_id: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Submission:
        return self._call("Submit", self._request(pb.SubmitRequest, task_id=task_id,
                          idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def put(self, resource: pb.Resource, *, idempotency_key: str, timeout: float | None = None) -> pb.Resource:
        return self._call("PutResource", self._request(pb.PutResourceRequest, resource=resource,
                          idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def inspect(self, kind: str, resource_id: str, *, timeout: float | None = None) -> pb.Resource:
        return self._call("GetResource", self._request(pb.GetResourceRequest, kind=kind, id=resource_id), timeout=timeout)

    def list(self, kind: str, *, after: str = "", limit: int = 100, timeout: float | None = None) -> pb.ListResourcesResponse:
        return self._call("ListResources", self._request(pb.ListResourcesRequest, kind=kind, after=after, limit=limit), timeout=timeout)

    def events(self, branch_id: str, *, after: int = 0, limit: int = 100, timeout: float | None = None) -> pb.EventsResponse:
        return self._call("Events", self._request(pb.EventsRequest, branch_id=branch_id, after_sequence=after, limit=limit), timeout=timeout)

    def action(self, branch_id: str, action: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Resource:
        return self._call("Action", self._request(pb.ActionRequest, branch_id=branch_id, action=action,
                          idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def suspend(self, branch_id: str, **kwargs) -> pb.Resource:
        return self.action(branch_id, "suspend", **kwargs)

    def resume(self, branch_id: str, **kwargs) -> pb.Resource:
        return self.action(branch_id, "resume", **kwargs)

    def cancel(self, branch_id: str, **kwargs) -> pb.Resource:
        return self.action(branch_id, "cancel", **kwargs)

    def read_file(self, branch_id: str, path: str, *, timeout: float | None = None) -> bytes:
        return self._call("ReadFile", self._request(pb.ReadFileRequest, branch_id=branch_id, path=path), timeout=timeout).data

    def write_file(self, branch_id: str, path: str, data: bytes, *, timeout: float | None = None) -> int:
        if len(data) > 1 << 20:
            raise ValueError("file exceeds 1 MiB")
        return self._call("WriteFile", self._request(pb.WriteFileRequest, branch_id=branch_id, path=path, data=data), timeout=timeout, mutation=True).bytes_written

    def exec(self, branch_id: str, argv: list[str], *, cwd: str = "", env: dict[str, str] | None = None,
             timeout: float | None = None) -> Iterator[guest.ExecEvent]:
        options = self._kwargs(timeout)
        request = self._request(pb.ExecRequest, branch_id=branch_id, command=guest.ExecRequest(
            argv=argv, cwd=cwd, env=env or {}, timeout_ms=int(options["timeout"] * 1000)))
        call = self._stub.Exec(request, **options)
        result = False
        try:
            for event in call:
                if result:
                    raise HypercallError("data_loss", "command emitted events after its result")
                result = event.HasField("result")
                yield event
            if not result:
                raise HypercallError("data_loss", "command ended without a result", ambiguous=True)
        except grpc.RpcError as error:
            raise HypercallError.from_rpc(error, mutation=True) from error
        finally:
            call.cancel()

    def run_command(self, branch_id: str, argv: list[str], **kwargs) -> CommandResult:
        stdout, stderr, result = bytearray(), bytearray(), None
        for event in self.exec(branch_id, argv, **kwargs):
            if event.HasField("stdout"):
                stdout.extend(event.stdout)
            elif event.HasField("stderr"):
                stderr.extend(event.stderr)
            else:
                result = event.result
        assert result is not None
        return CommandResult(bytes(stdout), bytes(stderr), result.exit_code, result.timed_out, result.output_truncated)

    def fork(self, checkpoint_id: str, models: list[pb.Ref], *, idempotency_key: str,
             timeout: float | None = None) -> pb.ForkResult:
        return self._call("Fork", self._request(pb.ForkRequest, checkpoint_id=checkpoint_id, models=models,
                          idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    def calls(self, branch_id: str, *, after: int = 0, limit: int = 100,
              timeout: float | None = None) -> pb.CallsResponse:
        return self._call("Calls", self._request(pb.CallsRequest, branch_id=branch_id,
                          after_sequence=after, limit=limit), timeout=timeout)

    def steps(self, branch_id: str, *, after: int = 0, limit: int = 100,
              timeout: float | None = None) -> pb.StepsResponse:
        return self._call("Steps", self._request(pb.StepsRequest, branch_id=branch_id,
                          after_step=after, limit=limit), timeout=timeout)

    def iter_calls(self, branch_id: str, *, after: int = 0, page_size: int = 100,
                   timeout: float | None = None) -> Iterator[pb.AgentCall]:
        while True:
            page = self.calls(branch_id, after=after, limit=page_size, timeout=timeout)
            if not page.calls:
                return
            if page.next_sequence <= after:
                raise HypercallError("data_loss", "call cursor did not advance")
            yield from page.calls
            after = page.next_sequence

    def iter_steps(self, branch_id: str, *, after: int = 0, page_size: int = 100,
                   timeout: float | None = None) -> Iterator[pb.AgentStep]:
        while True:
            page = self.steps(branch_id, after=after, limit=page_size, timeout=timeout)
            if not page.steps:
                return
            if page.next_step <= after:
                raise HypercallError("data_loss", "step cursor did not advance")
            yield from page.steps
            after = page.next_step

    def compare(self, left_branch: str, right_branch: str, *, comparator: str,
                timeout: float | None = None) -> pb.Divergence:
        return self._call("Compare", self._request(pb.CompareRequest, left_branch=left_branch,
                          right_branch=right_branch, comparator=comparator), timeout=timeout)


class AsyncClient(_Configuration):
    async def compile_evaluation(self, config: str, *, timeout: float | None = None) -> pb.EvaluationPlan:
        return await self._call("CompileEvaluation", self._request(pb.CompileEvaluationRequest, config_yaml=config), timeout=timeout)

    async def evaluate(self, config: str, observations: list[pb.EvaluationObservation], *, idempotency_key: str,
                       timeout: float | None = None) -> pb.Evaluation:
        return await self._call("SubmitEvaluation", self._request(pb.SubmitEvaluationRequest, config_yaml=config,
                                observations=observations, idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def evaluation(self, evaluation_id: str, *, timeout: float | None = None) -> pb.Evaluation:
        return await self._call("GetEvaluation", self._request(pb.GetEvaluationRequest, id=evaluation_id), timeout=timeout)

    async def report(self, evaluation_id: str, *, timeout: float | None = None) -> pb.EvaluationReport:
        return await self._call("GetReport", self._request(pb.GetReportRequest, id=evaluation_id), timeout=timeout)

    async def projection_status(self, *, timeout: float | None = None) -> pb.ProjectionStatusResponse:
        return await self._call("ProjectionStatus", self._request(pb.ProjectionStatusRequest), timeout=timeout)

    def __init__(self, endpoint: str, **kwargs):
        super().__init__(endpoint, **kwargs)
        options = (("grpc.enable_retries", 0), ("grpc.max_receive_message_length", 4 << 20))
        self._channel = (grpc.aio.insecure_channel(endpoint, options=options) if self._plaintext
                         else grpc.aio.secure_channel(endpoint, grpc.ssl_channel_credentials(self._roots), options=options))
        self._stub = ControlPlaneStub(self._channel)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *_):
        await self.close()

    async def close(self) -> None:
        await self._channel.close()

    async def _call(self, method: str, request, *, timeout: float | None = None, mutation: bool = False):
        try:
            return await getattr(self._stub, method)(request, **self._kwargs(timeout))
        except grpc.RpcError as error:
            raise HypercallError.from_rpc(error, mutation=mutation) from error

    async def submit_yaml(self, manifest: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Submission:
        return await self._call("SubmitManifest", self._request(pb.SubmitManifestRequest, manifest_yaml=manifest,
                                idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def submit(self, task_id: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Submission:
        return await self._call("Submit", self._request(pb.SubmitRequest, task_id=task_id,
                                idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def put(self, resource: pb.Resource, *, idempotency_key: str, timeout: float | None = None) -> pb.Resource:
        return await self._call("PutResource", self._request(pb.PutResourceRequest, resource=resource,
                                idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def inspect(self, kind: str, resource_id: str, *, timeout: float | None = None) -> pb.Resource:
        return await self._call("GetResource", self._request(pb.GetResourceRequest, kind=kind, id=resource_id), timeout=timeout)

    async def list(self, kind: str, *, after: str = "", limit: int = 100, timeout: float | None = None) -> pb.ListResourcesResponse:
        return await self._call("ListResources", self._request(pb.ListResourcesRequest, kind=kind, after=after, limit=limit), timeout=timeout)

    async def events(self, branch_id: str, *, after: int = 0, limit: int = 100, timeout: float | None = None) -> pb.EventsResponse:
        return await self._call("Events", self._request(pb.EventsRequest, branch_id=branch_id, after_sequence=after, limit=limit), timeout=timeout)

    async def action(self, branch_id: str, action: str, *, idempotency_key: str, timeout: float | None = None) -> pb.Resource:
        return await self._call("Action", self._request(pb.ActionRequest, branch_id=branch_id, action=action,
                                idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def suspend(self, branch_id: str, **kwargs) -> pb.Resource:
        return await self.action(branch_id, "suspend", **kwargs)

    async def resume(self, branch_id: str, **kwargs) -> pb.Resource:
        return await self.action(branch_id, "resume", **kwargs)

    async def cancel(self, branch_id: str, **kwargs) -> pb.Resource:
        return await self.action(branch_id, "cancel", **kwargs)

    async def read_file(self, branch_id: str, path: str, *, timeout: float | None = None) -> bytes:
        return (await self._call("ReadFile", self._request(pb.ReadFileRequest, branch_id=branch_id, path=path), timeout=timeout)).data

    async def write_file(self, branch_id: str, path: str, data: bytes, *, timeout: float | None = None) -> int:
        if len(data) > 1 << 20:
            raise ValueError("file exceeds 1 MiB")
        return (await self._call("WriteFile", self._request(pb.WriteFileRequest, branch_id=branch_id, path=path, data=data), timeout=timeout, mutation=True)).bytes_written

    async def exec(self, branch_id: str, argv: list[str], *, cwd: str = "", env: dict[str, str] | None = None,
                   timeout: float | None = None) -> AsyncIterator[guest.ExecEvent]:
        options = self._kwargs(timeout)
        request = self._request(pb.ExecRequest, branch_id=branch_id, command=guest.ExecRequest(
            argv=argv, cwd=cwd, env=env or {}, timeout_ms=int(options["timeout"] * 1000)))
        call = self._stub.Exec(request, **options)
        result = False
        try:
            async for event in call:
                if result:
                    raise HypercallError("data_loss", "command emitted events after its result")
                result = event.HasField("result")
                yield event
            if not result:
                raise HypercallError("data_loss", "command ended without a result", ambiguous=True)
        except grpc.RpcError as error:
            raise HypercallError.from_rpc(error, mutation=True) from error
        finally:
            call.cancel()

    async def run_command(self, branch_id: str, argv: list[str], **kwargs) -> CommandResult:
        stdout, stderr, result = bytearray(), bytearray(), None
        async for event in self.exec(branch_id, argv, **kwargs):
            if event.HasField("stdout"):
                stdout.extend(event.stdout)
            elif event.HasField("stderr"):
                stderr.extend(event.stderr)
            else:
                result = event.result
        assert result is not None
        return CommandResult(bytes(stdout), bytes(stderr), result.exit_code, result.timed_out, result.output_truncated)

    async def fork(self, checkpoint_id: str, models: list[pb.Ref], *, idempotency_key: str,
                   timeout: float | None = None) -> pb.ForkResult:
        return await self._call("Fork", self._request(pb.ForkRequest, checkpoint_id=checkpoint_id, models=models,
                                idempotency_key=idempotency_key), timeout=timeout, mutation=True)

    async def calls(self, branch_id: str, *, after: int = 0, limit: int = 100,
                    timeout: float | None = None) -> pb.CallsResponse:
        return await self._call("Calls", self._request(pb.CallsRequest, branch_id=branch_id,
                                after_sequence=after, limit=limit), timeout=timeout)

    async def steps(self, branch_id: str, *, after: int = 0, limit: int = 100,
                    timeout: float | None = None) -> pb.StepsResponse:
        return await self._call("Steps", self._request(pb.StepsRequest, branch_id=branch_id,
                                after_step=after, limit=limit), timeout=timeout)

    async def iter_calls(self, branch_id: str, *, after: int = 0, page_size: int = 100,
                         timeout: float | None = None) -> AsyncIterator[pb.AgentCall]:
        while True:
            page = await self.calls(branch_id, after=after, limit=page_size, timeout=timeout)
            if not page.calls:
                return
            if page.next_sequence <= after:
                raise HypercallError("data_loss", "call cursor did not advance")
            for call in page.calls:
                yield call
            after = page.next_sequence

    async def iter_steps(self, branch_id: str, *, after: int = 0, page_size: int = 100,
                         timeout: float | None = None) -> AsyncIterator[pb.AgentStep]:
        while True:
            page = await self.steps(branch_id, after=after, limit=page_size, timeout=timeout)
            if not page.steps:
                return
            if page.next_step <= after:
                raise HypercallError("data_loss", "step cursor did not advance")
            for step in page.steps:
                yield step
            after = page.next_step

    async def compare(self, left_branch: str, right_branch: str, *, comparator: str,
                      timeout: float | None = None) -> pb.Divergence:
        return await self._call("Compare", self._request(pb.CompareRequest, left_branch=left_branch,
                                right_branch=right_branch, comparator=comparator), timeout=timeout)
