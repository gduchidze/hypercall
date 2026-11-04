"""Single-invocation gRPC worker inside a restricted OCI container."""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
import hashlib
import hmac
import json
import subprocess
import sys
import math
import os
from pathlib import Path
import re
import threading

import grpc
from hypercall_proto.hypercall.scorer.v1 import scorer_pb2, scorer_pb2_grpc

MAX_MESSAGE = 1024 * 1024
DIGEST = re.compile(r"sha256:[0-9a-f]{64}\Z")


def verified(path: str, expected: str) -> bytes:
    data = Path(path).read_bytes()
    if len(data) > MAX_MESSAGE or not DIGEST.fullmatch(expected):
        raise ValueError("invalid artifact")
    if "sha256:" + hashlib.sha256(data).hexdigest() != expected:
        raise ValueError("artifact digest mismatch")
    return data


class Worker(scorer_pb2_grpc.ScorerServicer):
    def __init__(self):
        self.used = False
        self.lock = threading.Lock()

    def Score(self, request, context):
        supplied = dict(context.invocation_metadata()).get("x-hypercall-nonce", "")
        if not hmac.compare_digest(supplied, os.environ["HYPERCALL_SCORER_NONCE"]):
            context.abort(grpc.StatusCode.UNAUTHENTICATED, "invalid worker identity")
        with self.lock:
            if self.used:
                context.abort(grpc.StatusCode.FAILED_PRECONDITION, "worker already used")
            self.used = True
        try:
            if request.ByteSize() > MAX_MESSAGE:
                raise ValueError("request too large")
            if request.scorer_digest != os.environ["HYPERCALL_SCORER_DIGEST"]:
                raise ValueError("scorer digest mismatch")
            verified("/artifact/scorer.py", request.scorer_digest)
            rubric = ""
            configured_rubric = os.environ.get("HYPERCALL_RUBRIC_DIGEST", "")
            if request.rubric_digest != configured_rubric:
                raise ValueError("rubric digest mismatch")
            if request.rubric_digest:
                rubric = verified("/artifact/rubric.txt", request.rubric_digest).decode("utf-8")
            if request.judge_model_digest and (not DIGEST.fullmatch(request.judge_model_digest) or not rubric or not request.parameters):
                raise ValueError("judge requires pinned model, rubric and parameters")
            # A clean child avoids forking from a process that imported gRPC.
            # Absolute executable and close_fds=False select posix_spawn; Python
            # descriptors remain CLOEXEC. No server nonce is inherited in env.
            child_request = {
                "output": request.output, "variables": dict(request.variables),
                "scorer_digest": request.scorer_digest, "rubric_digest": request.rubric_digest,
                "judge_model_digest": request.judge_model_digest,
                "parameters": dict(request.parameters), "rubric": rubric,
            }
            completed = subprocess.run(
                [sys.executable, "-m", "hypercall_scorer.invoke"],
                input=json.dumps(child_request, ensure_ascii=False).encode(),
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, close_fds=False,
                env={"PYTHONPATH": "/opt", "HOME": "/tmp", "PATH": "/usr/local/bin:/usr/bin:/bin",
                     "PYTHONDONTWRITEBYTECODE": "1"},
            )
            if completed.returncode:
                return scorer_pb2.ScoreResult(error_code="worker_error", reason="scorer process exited without a result")
            result_path = Path("/tmp/result.json")
            if not result_path.is_file() or result_path.stat().st_size > MAX_MESSAGE:
                return scorer_pb2.ScoreResult(error_code="malformed_result", reason="missing or oversized scorer result")
            with result_path.open(encoding="utf-8") as result_file:
                envelope = json.load(result_file)
            if envelope.get("error"):
                return scorer_pb2.ScoreResult(error_code="scorer_error", reason=str(envelope.get("reason", "scorer failed"))[:8192])
            result = envelope.get("result")
        except Exception as error:
            return scorer_pb2.ScoreResult(error_code="scorer_error", reason=f"scorer failed: {type(error).__name__}: {str(error)[:1024]}")
        try:
            if not isinstance(result, dict) or set(result) - {"pass", "score", "reason", "cost"}:
                raise ValueError("invalid result fields")
            if type(result.get("pass")) is not bool or type(result.get("score")) not in (int, float):
                raise ValueError("missing score or pass")
            score = float(result["score"])
            if not math.isfinite(score) or not 0 <= score <= 1:
                raise ValueError("invalid score")
            reason = result.get("reason", "")
            if not isinstance(reason, str) or len(reason.encode()) > 8192:
                raise ValueError("invalid reason")
            response = scorer_pb2.ScoreResult()
            setattr(response, "pass", result["pass"])
            response.score, response.reason = score, reason
            if result.get("cost") is not None:
                cost = result["cost"]
                if set(cost) != {"amount", "currency", "kind", "source"} or type(cost["amount"]) not in (int, float):
                    raise ValueError("invalid cost")
                if not math.isfinite(cost["amount"]) or cost["amount"] < 0 or cost["kind"] not in ("measured", "estimated", "synthetic"):
                    raise ValueError("invalid cost")
                if not re.fullmatch(r"[A-Z]{3}", cost["currency"]) or not 0 < len(cost["source"].encode()) <= 1024:
                    raise ValueError("invalid cost provenance")
                response.cost.CopyFrom(scorer_pb2.Cost(**cost))
            return response
        except Exception:
            return scorer_pb2.ScoreResult(error_code="malformed_result", reason="scorer result violates contract")


def main():
    os.umask(0o077)
    server = grpc.server(ThreadPoolExecutor(max_workers=1), options=[
        ("grpc.max_receive_message_length", MAX_MESSAGE),
        ("grpc.max_send_message_length", MAX_MESSAGE),
    ], maximum_concurrent_rpcs=1)
    scorer_pb2_grpc.add_ScorerServicer_to_server(Worker(), server)
    if not server.add_insecure_port("unix:/tmp/scorer.sock"):
        raise RuntimeError("cannot bind scorer socket")
    server.start()
    server.wait_for_termination()


if __name__ == "__main__":
    main()
