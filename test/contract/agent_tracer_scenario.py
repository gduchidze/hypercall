"""Real guest-local Go RPC and Postgres fixture tracer; no VM qualification."""
import json
import sys

from hypercall.harness import AgentHarness

with AgentHarness(sys.argv[1], timeout=10) as agent:
    response = agent.tool("echo", b"shared-prefix", step=1, position=1)
    assert response.payload == b"shared-prefix" and response.state == "completed"
    barrier = agent.commit_step(1, b'{"prefix":"shared-prefix"}', position=2)
    assert barrier.event_sequence > response.event_sequence
    print(json.dumps({"step": barrier.step, "event_sequence": barrier.event_sequence,
                      "state_digest": barrier.state_digest, "session_hash": barrier.session_hash}))
