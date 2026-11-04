"""Public sync/async SDK and compiled CLI trajectory/fork contract; no VMs."""
import asyncio
import json
import os
import subprocess
import sys

from hypercall import AsyncClient, Client, types

endpoint, project, branch, checkpoint, model_id, model_digest, operation_id = sys.argv[1:]
options = dict(project=project, api_key=os.environ["HYPERCALL_API_KEY"], plaintext=True)
models = [types.Ref(id=model_id, digest=model_digest), types.Ref(id=model_id, digest=model_digest)]
with Client(endpoint, **options) as client:
    calls = list(client.iter_calls(branch, page_size=1))
    assert len(calls) == 2 and [call.request.kind for call in calls] == ["tool", "tool"]
    assert calls[1].registered_sequence > calls[0].registered_sequence
    steps = list(client.iter_steps(branch, page_size=1))
    assert len(steps) == 1 and steps[0].complete
    fork = client.fork(checkpoint, models, idempotency_key="public-fork")
    assert fork.operation.metadata.id == operation_id and len(fork.children) == 2
    child_ids = [child.metadata.id for child in fork.children]
    comparison = client.compare(*child_ids, comparator="output_bytes_v1")
    assert comparison.state in {"unknown", "incomplete"} and comparison.reason

async def async_contract():
    async with AsyncClient(endpoint, **options) as client:
        assert len([call async for call in client.iter_calls(branch, page_size=1)]) == 2
        assert len([step async for step in client.iter_steps(branch, page_size=1)]) == 1
        assert (await client.fork(checkpoint, models, idempotency_key="public-fork")).operation.metadata.id == operation_id
        assert (await client.compare(*child_ids, comparator="observations_v1")).state in {"unknown", "incomplete"}
asyncio.run(async_contract())

def cli(*arguments, key=None):
    command = [os.environ["HYPERCALL_TEST_CLI"], "--endpoint", endpoint, "--project", project, "--plaintext", "--limit", "1"]
    if key:
        command += ["--key", key]
    result = subprocess.run(command + list(arguments), capture_output=True, text=True, check=True)
    return json.loads(result.stdout)

assert len(cli("calls", branch)["calls"]) == 1
assert len(cli("steps", branch)["steps"]) == 1
candidate = model_id + "@" + model_digest
assert cli("fork", checkpoint, candidate, candidate, key="public-fork")["operation"]["metadata"]["id"] == operation_id
assert cli("compare", *child_ids, "output_normalized_v1")["state"] in {"unknown", "incomplete"}
print("public sync/async SDK and CLI paginated traces, immutable fork replay, incomplete comparison passed")
