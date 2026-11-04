"""Pinned cooperative fixture agent: tool prefix, routed inference, observation.

This is an instrumented agent process, not the Phase 1B counter workload. All
inference/tool execution is registered with the durable control-plane gateway.
"""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path

from .client import HypercallError
from .harness import AgentHarness


def state_file(value: dict) -> bytes:
    data = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    temporary = Path(".agent-state.pending")
    with temporary.open("wb") as output:
        output.write(data)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, "agent-state.json")
    descriptor = os.open(".", os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    return data


def run(socket_path: str) -> None:
    with AgentHarness(socket_path, timeout=60) as agent:
        cached_parent_model = agent.model_client()
        prefix = agent.tool("echo", b"shared-prefix", step=1, position=1)
        committed = agent.commit_step(1, state_file({"step": 1, "prefix": prefix.payload.decode()}), position=2)
        parent_hash = agent.session.session_hash
        session = agent.await_resume(committed)
        try:
            cached_parent_model.generate(b"continue shared-prefix", step=2, position=1)
        except HypercallError as error:
            if error.code != "failed_precondition":
                raise
        else:
            raise RuntimeError("inherited model client escaped the resume hook")
        response = agent.model_client().generate(b"continue shared-prefix", step=2, position=1)
        agent.commit_step(2, state_file({"step": 2, "output": response.payload.decode("utf-8"),
                                        "parent_session_hash": parent_hash, "session_hash": session.session_hash,
                                        "stale_client_rejected": True}), position=2)
        observation = agent.tool("echo", response.payload, step=3, position=1)
        final = state_file({"step": 3, "output": response.payload.decode("utf-8"),
                            "observation": observation.payload.decode("utf-8"), "stale_client_rejected": True})
        agent.commit_step(3, final, position=2)
        agent.finalize(3, response.payload, position=3)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socket", default="/run/hypercall/harness.sock")
    args = parser.parse_args()
    run(args.socket)


if __name__ == "__main__":
    main()
