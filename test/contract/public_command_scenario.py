"""Public SDK byte/stream contract against a test-only backend; never a VM test."""
import asyncio
import os
import sys

from hypercall import AsyncClient, Client, HypercallError

endpoint, project, branch = sys.argv[1:4]
options = dict(project=project, api_key=os.environ["HYPERCALL_API_KEY"], plaintext=True)
with Client(endpoint, **options) as client:
    content = b"\x00\xff\nUTF-8: \xe2\x98\x83"
    assert client.write_file(branch, "binary.dat", content) == len(content)
    assert client.read_file(branch, "binary.dat") == content
    result = client.run_command(branch, ["fixture"])
    assert result.stdout == b"fixture output\x00"
    assert result.stderr == b"fixture stderr\n"
    assert result.exit_code == 7 and not result.timed_out
    for command, code in (("wrapped-cancel", "cancelled"), ("wrapped-deadline", "deadline_exceeded")):
        try:
            client.run_command(branch, [command])
        except HypercallError as error:
            assert error.code == code, error
            assert error.ambiguous
        else:
            raise AssertionError("structured backend status was lost")
    try:
        client.run_command(branch, ["wait"], timeout=0.05)
    except HypercallError as error:
        assert error.code in {"deadline_exceeded", "cancelled"}, error
        assert error.ambiguous
    else:
        raise AssertionError("deadline did not cancel command stream")

async def check_async():
    async with AsyncClient(endpoint, **options) as client:
        assert await client.read_file(branch, "binary.dat") == content
        result = await client.run_command(branch, ["fixture"])
        assert result.stdout == b"fixture output\x00" and result.exit_code == 7
asyncio.run(check_async())
print("sync/async SDK bytes, command terminal result, and cancellation passed (test-only backend)")
