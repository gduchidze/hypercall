"""Invoked against the real Postgres-backed authenticated API by Go integration tests."""
import asyncio
import json
import os
from pathlib import Path
import subprocess
import sys

from hypercall import Client, AsyncClient, HypercallError

endpoint, project = sys.argv[1:3]
manifest = Path("test/fixtures/phase2a/task.yaml").read_text()

with Client(endpoint, project=project, api_key=os.environ["HYPERCALL_API_KEY"], plaintext=True) as client:
    submitted = client.submit_yaml(manifest, idempotency_key="python-submit")
    run_id = submitted.run.metadata.id
    assert client.submit_yaml(manifest, idempotency_key="python-submit").run.metadata.id == run_id
    assert client.inspect("Run", run_id).run_spec.task.task_spec.workspace.digest.startswith("sha256:")
    events = client.events(submitted.branch.metadata.id)
    assert events.events[0].sequence == 1
    assert not client.events(submitted.branch.metadata.id, after=events.next_sequence).events
    # Capability admission fails before persisting lifecycle intent on this
    # deliberately snapshot-incapable protocol fixture.
    for action in ("suspend", "resume"):
        try:
            client.action(submitted.branch.metadata.id, action, idempotency_key="unsupported-" + action)
        except HypercallError as error:
            assert error.code == "failed_precondition", error
        else:
            raise AssertionError("snapshot-incapable backend admitted " + action)
    assert client.events(submitted.branch.metadata.id).next_sequence == events.next_sequence
    try:
        client.read_file(submitted.branch.metadata.id, "output.txt")
    except HypercallError as error:
        assert error.code == "failed_precondition", error
        assert not error.retryable
    else:
        raise AssertionError("unavailable runtime unexpectedly allowed files")
    try:
        client.submit_yaml(manifest.replace("private_writes: true", "private_writes: true\n  typo: true"), idempotency_key="typo")
    except HypercallError as error:
        assert error.code == "invalid_argument", error
    else:
        raise AssertionError("unknown manifest field accepted")

async def check_async():
    async with AsyncClient(endpoint, project=project, api_key=os.environ["HYPERCALL_API_KEY"], plaintext=True) as client:
        result = await client.submit_yaml(manifest, idempotency_key="python-submit")
        assert result.run.metadata.id == run_id
        assert (await client.inspect("Run", run_id)).metadata.id == run_id
        assert (await client.events(result.branch.metadata.id)).events[0].sequence == 1
asyncio.run(check_async())

cli = subprocess.run([os.environ["HYPERCALL_TEST_CLI"], "--endpoint", endpoint, "--project", project,
                      "--plaintext", "--key", "cli-submit", "submit", "test/fixtures/phase2a/task.yaml"],
                     check=True, capture_output=True, text=True)
result = json.loads(cli.stdout)
assert result["run"]["metadata"]["id"]
inspect = subprocess.run([os.environ["HYPERCALL_TEST_CLI"], "--endpoint", endpoint, "--project", project,
                          "--plaintext", "inspect", "Run", result["run"]["metadata"]["id"]],
                         check=True, capture_output=True, text=True)
assert json.loads(inspect.stdout)["metadata"]["id"] == result["run"]["metadata"]["id"]
print("sync/async Python SDK and Go CLI authenticated YAML submission passed")
