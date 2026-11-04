FROM python:3.12.12-slim-bookworm@sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 HOME=/tmp
COPY deploy/scorer/requirements.lock /opt/requirements.lock
RUN pip install --no-cache-dir --only-binary=:all: --require-hashes -r /opt/requirements.lock
COPY python/hypercall_proto/__init__.py /opt/hypercall_proto/__init__.py
COPY python/hypercall_proto/hypercall/__init__.py /opt/hypercall_proto/hypercall/__init__.py
COPY python/hypercall_proto/hypercall/scorer/__init__.py /opt/hypercall_proto/hypercall/scorer/__init__.py
COPY python/hypercall_proto/hypercall/scorer/v1/*.py /opt/hypercall_proto/hypercall/scorer/v1/
COPY python/hypercall_scorer/*.py /opt/hypercall_scorer/
COPY LICENSE /opt/notices/
ENV PYTHONPATH=/opt
USER 65532:65532
ENTRYPOINT ["python", "-m", "hypercall_scorer.supervise"]
