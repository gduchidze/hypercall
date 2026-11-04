"""Clean Python process for one untrusted scorer invocation (no gRPC runtime)."""
from __future__ import annotations
import hashlib
import importlib.util
import json
from pathlib import Path
import sys


def main():
    request = json.load(sys.stdin)
    code = Path("/artifact/scorer.py").read_bytes()
    if "sha256:" + hashlib.sha256(code).hexdigest() != request["scorer_digest"]:
        raise ValueError("artifact digest mismatch")
    try:
        spec = importlib.util.spec_from_file_location("isolated_scorer", "/artifact/scorer.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        envelope = {"result": module.score(request)}
    except Exception as error:
        envelope = {"error": "scorer_error", "reason": f"scorer failed: {type(error).__name__}: {str(error)[:1024]}"}
    # Scorer stdout/stderr go to /dev/null. Results use bounded tmpfs and are
    # revalidated by the gRPC parent and the host before becoming scores.
    with open("/tmp/result.json", "w", encoding="utf-8") as result:
        json.dump(envelope, result, ensure_ascii=False)


if __name__ == "__main__":
    main()
