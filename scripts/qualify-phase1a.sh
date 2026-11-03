#!/bin/sh
# Run on the explicitly qualified Linux KVM host as root. Artifacts, state and
# delegated cgroup parent are mandatory. This script never downloads artifacts.
set -eu
: "${HYPERCALL_HOST_CONFIG:?set the absolute pinned runtime.json path}"
: "${HYPERCALL_CGROUP_DIR:?set a dedicated delegated cgroups v2 parent}"
: "${HYPERCALL_QUALIFICATION_STATE:?set a private dedicated runtime state path}"
: "${HYPERCALL_PYTHON:?set the locked Python executable path}"
: "${HYPERCALL_PYTHON_PROBE:?set the absolute scripts/qualify-python.py path}"
: "${HYPERCALL_TESTDRIVER:?set the absolute built hypercall-testdriver path}"
export HYPERCALL_KVM_TEST=1
exec go test -tags=kvm -count=1 -v -timeout=9m ./test/integration/kvm
