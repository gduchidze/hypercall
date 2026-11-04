"""Trusted container PID 1: enforce lifetime even after evaluator/client death."""
from __future__ import annotations

import ctypes
import math
import os
import subprocess
import sys


def main():
    # Fixed operator-supplied environment, never request or scorer-controlled.
    lifetime = float(os.environ["HYPERCALL_SCORER_LIFETIME_SECONDS"])
    if not math.isfinite(lifetime) or not 0 < lifetime <= 3600:
        os._exit(125)
    # The untrusted descendant shares our UID but cannot ptrace or rewrite the
    # watchdog through /proc even on kernels with permissive ptrace defaults.
    if ctypes.CDLL(None).prctl(4, 0, 0, 0, 0) != 0:  # PR_SET_DUMPABLE=0
        os._exit(125)
    worker = subprocess.Popen([sys.executable, "-m", "hypercall_scorer.worker"], close_fds=False)
    try:
        status = worker.wait(timeout=lifetime)
    except subprocess.TimeoutExpired:
        # Exiting namespace PID 1 makes the Linux kernel SIGKILL every remaining
        # process, including detached descendants and scorers ignoring signals.
        os._exit(124)
    os._exit(status if status >= 0 else 128 - status)


if __name__ == "__main__":
    main()
