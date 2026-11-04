"""Exercise real public evaluation calls through both Python clients and Go CLI."""
import asyncio
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time

from hypercall import Client, AsyncClient
from hypercall_proto.hypercall.v1 import control_pb2 as pb

config = """apiVersion: hypercall.io/eval/v1
scope: whole_task
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates: [{id: fixture, model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}]
cases: [{id: case, assert: [{type: equals, value: hello}]}]
repetitions: 1
"""
endpoint, binary = sys.argv[1:]
options = dict(project="project", api_key="01234567890123456789012345678901", plaintext=True)
with Client(endpoint, **options) as client:
    plan = json.loads(client.compile_evaluation(config).plan_json)
    observation = pb.EvaluationObservation(cell_id=plan["cells"][0]["id"], source="synthetic", output="hello", state="complete", trace_complete=True)
    admitted = client.evaluate(config, [observation], idempotency_key="sdk-evaluation")
    assert client.evaluate(config, [observation], idempotency_key="sdk-evaluation").id == admitted.id
    for _ in range(200):
        if client.evaluation(admitted.id).state == "completed":
            break
        time.sleep(.01)
    assert client.report(admitted.id).ci_pass
    failures = []
    for index, state in enumerate(("execution_error", "evaluation_error", "missing")):
        evidence = [] if state == "missing" else [pb.EvaluationObservation(cell_id=plan["cells"][0]["id"], source="synthetic", state=state)]
        failed = client.evaluate(config, evidence, idempotency_key=f"sdk-error-{index}")
        failures.append((failed.id, "incomplete_matrix" if state == "missing" else state))
    budget_config = config + "policy:\n  max_cost_micro_usd: 0\n"
    failed = client.evaluate(budget_config, [pb.EvaluationObservation(
        cell_id=plan["cells"][0]["id"], source="synthetic", output="wrong", state="complete", trace_complete=True,
        measurements=pb.EvaluationMeasurements(cost=pb.EvaluationCost(micro_usd=1, kind="synthetic",
            price_digest="sha256:" + "d" * 64, recorded_at="2026-11-04T00:00:00Z")))], idempotency_key="sdk-budget")
    failures.append((failed.id, "budget_exceeded"))
    for identity, reason in failures:
        for _ in range(200):
            if client.evaluation(identity).state == "completed":
                break
            time.sleep(.01)
        report = client.report(identity)
        assert not report.ci_pass and reason in report.reasons

async def async_check():
    async with AsyncClient(endpoint, **options) as client:
        assert (await client.evaluation(admitted.id)).state == "completed"
        assert (await client.report(admitted.id)).ci_pass
        assert (await client.compile_evaluation(config)).digest == plan["digest"]
asyncio.run(async_check())

with tempfile.TemporaryDirectory() as directory:
    path = Path(directory) / "eval.yaml"
    path.write_text(config)
    env = dict(os.environ, HYPERCALL_ENDPOINT=endpoint, HYPERCALL_PROJECT="project", HYPERCALL_API_KEY=options["api_key"])
    result = subprocess.run([binary, "--plaintext", "--format", "markdown", "--check", "report", admitted.id], env=env, text=True, capture_output=True, check=True)
    assert "Evaluation report" in result.stdout
    for identity, reason in failures:
        result = subprocess.run([binary, "--plaintext", "--check", "report", identity], env=env, text=True, capture_output=True)
        assert result.returncode != 0 and reason in result.stdout and reason in result.stderr, result
print("public evaluation SDK sync/async and CLI report passed")
