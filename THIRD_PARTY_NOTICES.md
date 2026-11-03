# Third-party notices

Hypercall's original source is BSD-3-Clause licensed. The first Phase 1A wave is
an original implementation informed by the pinned upstreams below. No upstream
implementation or test fixture has been copied or translated. Generated Go and
Python bindings derive from Hypercall's original protobuf definition. This does
not claim E2B API compatibility, imported upstream tests, or runtime qualification.

| Project | Role in this tree | Preserved terms |
| --- | --- | --- |
| google/ax | Future control-plane reference | [Apache-2.0](third_party/licenses/ax-Apache-2.0.txt) |
| e2b-dev/runtime | Runtime/process/file behavior and architecture reference | [Apache-2.0](third_party/licenses/e2b-runtime-Apache-2.0.txt) |
| e2b-dev/E2B Python SDK | Command/file/lifecycle behavior reference | [Package MIT license](third_party/licenses/e2b-python-sdk-MIT.txt) |
| promptfoo/promptfoo | Future evaluator reference | [MIT](third_party/licenses/promptfoo-MIT.txt) |
| Firecracker | Host/jailer prerequisites reference | [Apache-2.0](third_party/licenses/firecracker-Apache-2.0.txt) |
| Lima | Optional local Linux development reference | [Apache-2.0](third_party/licenses/lima-Apache-2.0.txt), [NOTICE](third_party/licenses/lima-NOTICE.txt) |

Exact source commits, paths, local destinations, adaptations and verification
limits are recorded in [third_party/upstreams.yaml](third_party/upstreams.yaml).
The SDK package license is preserved separately from E2B's root license.

Go/Python dependencies are fetched through their package managers and pinned in
`go.mod`/`go.sum` and `pyproject.toml`/`uv.lock`. They are not vendored here. Before
shipping binaries, wheels or VM images, include their applicable dependency
notices and an SBOM; this reference ledger is not a completed distribution audit.
Any future source or fixture import must add its exact mapping and retain its
copyright, license and NOTICE material before the import ships.
