"""Durable public lifecycle conformance; the Go server uses no actual VM."""
import asyncio
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from hypercall import AsyncClient, Client, HypercallError

endpoint, project = sys.argv[1:3]
options = dict(project=project, api_key=os.environ["HYPERCALL_API_KEY"], plaintext=True)
client = Client(endpoint, **options)

def wait_for(kind, resource_id, predicate):
    until = time.monotonic() + 8
    while time.monotonic() < until:
        value = client.inspect(kind, resource_id)
        if predicate(value):
            return value
        time.sleep(0.025)
    raise AssertionError(f"timed out waiting for {kind}: {value}")

def wait_operation(operation):
    result = wait_for("Operation", operation.metadata.id,
                      lambda value: value.operation_status.state in {"succeeded", "failed", "superseded"})
    assert result.operation_status.state == "succeeded", result

def cli(action, key):
    result = subprocess.run([os.environ["HYPERCALL_TEST_CLI"], "--endpoint", endpoint,
                             "--project", project, "--plaintext", "--key", key, action, branch_id],
                            check=True, capture_output=True, text=True)
    return json.loads(result.stdout)

try:
    submitted = client.submit_yaml(Path("test/fixtures/phase2a/task.yaml").read_text(),
                                   idempotency_key="lifecycle-submit")
    branch_id = submitted.branch.metadata.id
    branch = wait_for("Branch", branch_id, lambda b: b.branch_status.observed == "running")
    first_epoch = branch.branch_status.epoch
    assert branch.branch_status.sandbox_id
    marker = b"retained state\x00\xff"
    assert client.write_file(branch_id, "marker.bin", marker) == len(marker)

    # Sync client: intent and selected checkpoint remain stable on retries.
    operation = client.suspend(branch_id, idempotency_key="sync-suspend")
    assert client.suspend(branch_id, idempotency_key="sync-suspend").metadata.id == operation.metadata.id
    wait_operation(operation)
    suspended = wait_for("Branch", branch_id, lambda b: b.branch_status.observed == "suspended")
    selected = suspended.branch_status.checkpoint_id
    assert selected and 0 < suspended.branch_status.budget_remaining_ms <= 30_000
    checkpoint = client.inspect("Checkpoint", selected).checkpoint_spec
    assert checkpoint.branch_id == branch_id and checkpoint.verified
    assert checkpoint.compatibility["snapshot_format"] == "test-only-state-v1"
    before = suspended.branch_status
    resumed = client.resume(branch_id, idempotency_key="sync-resume")
    assert resumed.operation_spec.checkpoint_id == selected
    assert client.resume(branch_id, idempotency_key="sync-resume").metadata.id == resumed.metadata.id
    wait_operation(resumed)
    branch = wait_for("Branch", branch_id, lambda b: b.branch_status.observed == "running")
    assert branch.branch_status.epoch > before.epoch
    assert branch.branch_status.execution_id != before.execution_id
    assert branch.branch_status.sandbox_id != before.sandbox_id
    assert client.read_file(branch_id, "marker.bin") == marker

    async def async_cycle():
        async with AsyncClient(endpoint, **options) as async_client:
            op = await async_client.suspend(branch_id, idempotency_key="async-suspend")
            assert (await async_client.suspend(branch_id, idempotency_key="async-suspend")).metadata.id == op.metadata.id
            await asyncio.to_thread(wait_operation, op)
            paused = await async_client.inspect("Branch", branch_id)
            assert paused.branch_status.observed == "suspended"
            op = await async_client.resume(branch_id, idempotency_key="async-resume")
            assert op.operation_spec.checkpoint_id == paused.branch_status.checkpoint_id
            assert (await async_client.resume(branch_id, idempotency_key="async-resume")).metadata.id == op.metadata.id
            await asyncio.to_thread(wait_operation, op)
            running = await async_client.inspect("Branch", branch_id)
            assert running.branch_status.execution_id != paused.branch_status.execution_id
            assert running.branch_status.sandbox_id != paused.branch_status.sandbox_id
            assert running.branch_status.epoch > paused.branch_status.epoch
            assert await async_client.read_file(branch_id, "marker.bin") == marker
    asyncio.run(async_cycle())

    # CLI exercises its own action parsing and generated status output.
    paused_op = cli("suspend", "cli-suspend")
    wait_for("Operation", paused_op["metadata"]["id"], lambda op: op.operation_status.state == "succeeded")
    paused = client.inspect("Branch", branch_id)
    resumed_op = cli("resume", "cli-resume")
    assert resumed_op["operation_spec"]["checkpoint_id"] == paused.branch_status.checkpoint_id
    wait_for("Operation", resumed_op["metadata"]["id"], lambda op: op.operation_status.state == "succeeded")
    assert client.read_file(branch_id, "marker.bin") == marker
    branch = client.inspect("Branch", branch_id)
    assert branch.branch_status.sandbox_id != paused.branch_status.sandbox_id
    assert branch.branch_status.epoch >= first_epoch + 3
    events = client.events(branch_id, limit=1000)
    assert [event.sequence for event in events.events] == list(range(1, events.next_sequence + 1))
    assert sum(event.type == "suspended" for event in events.events) == 3
    assert sum(event.type == "running" for event in events.events) >= 4
    cancel = client.cancel(branch_id, idempotency_key="lifecycle-cancel")
    wait_operation(cancel)
    assert client.inspect("Branch", branch_id).branch_status.observed == "cancelled"
    try:
        client.resume(branch_id, idempotency_key="terminal-resume")
    except HypercallError as error:
        assert error.code == "failed_precondition", error
    else:
        raise AssertionError("terminal branch resumed")
    print("sync/async SDK and CLI retained-checkpoint lifecycle passed (state conformance; no VM)")
finally:
    client.close()
