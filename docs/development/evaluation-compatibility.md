# Supported evaluation contract

Hypercall accepts `hypercall.io/eval/v1` manifests with immutable workload/model
digests and an explicit `whole_task` or `continuation` scope. Unknown fields fail
admission. Configuration is data and never executes arbitrary code or templates.

## Supported behavior

| Area | Contract |
| --- | --- |
| Matrix | Named candidates × cases × nonempty string-array variable dimensions × positive repetitions. Scalar case variables are retained. Variable keys sort for stable expansion and hashes. |
| Equality | `equals` / `not-equals`: exact strings, primitive string conversion, JSON object/array/null equality, and negated equality on non-JSON output. An explicit value is required. |
| Containment | `contains` / `not-contains`: nonempty text or finite numeric value, case sensitive. |
| JSON | `is-json` / `not-is-json`: complete valid JSON or a schema using the subset below. |
| Numeric | `latency` / `not-latency` in milliseconds and `cost` / `not-cost` in USD with an explicit nonnegative threshold. Positive assertions use `<=`; negated assertions use its inverse. Costs retain integer microUSD and price provenance. |
| Variables | Literal `{{ name }}` substitution from declared case variables, including assertion JSON values. Missing variables, filters, calls, file expressions and arbitrary templates fail admission. |
| Weighting | Nonnegative finite assertion weights and a weighted mean score. Zero weight is metric only: an otherwise valid component passes while preserving its raw score. |
| Case threshold | A finite value in `[0,1]` overrides ordinary assertion pass/fail using aggregate score. Zero is honored. Execution and evaluation errors remain failures. |
| Unicode | Exact text comparisons preserve Unicode bytes without implicit normalization. |

The JSON schema subset supports a single `type`: `object`, `array`, `string`,
`number`, `integer`, `boolean`, or `null`; nested `properties`; `required`; boolean
`additionalProperties`; `enum`; one schema for `items`; and inclusive `minimum`
and `maximum`. Other keywords fail admission. Remote/file schemas, `$ref`, regex
patterns, formats, combinators and schema-code execution are unsupported.

## Input and failure rules

Each case requires 1..64 explicit assertions. Missing equality values, undeclared
variables, Unicode-normalization options, empty or nonstring array dimensions,
unsupported assertion extensions and provider plugins fail admission. Explicit
JSON null equality is supported.

Missing numeric measurements become nonpassing `evaluation_error` results.
Weighted-score overflow fails with score zero. Missing observations, execution
errors, timeouts, scorer errors and incomplete traces cannot pass by a threshold
or zero-weight override. Missing and errored cells remain in the expected
pass-rate denominator. Score means use evaluated samples separately.

Custom `python` and `llm-judge` assertions use the isolated scorer gRPC contract.
Scorer, rubric and judge identities must be pinned. Deterministic scorer fixtures
establish protocol and failure behavior; live model/judge qualification belongs
to Phase 4A.

Admission limits are 1 MiB source YAML, 1,000 cells, 128 candidates, 4,000 resolved
assertion applications, 2 MiB resolved matrix/input, and depth 16. A resolved plan
plus retained baseline is limited to 3 MiB. Reasons and scorer cost-source details
share a 128 KiB budget measured after JSON escaping. Truncation flags and source
digests preserve omission identity while retaining scores, errors and amounts.
These limits keep report and inspection responses within the public 4 MiB bound.

## Fixed conformance corpus

The versioned fixtures in `test/fixtures/evaluation/expected.json` retain fixed
expected values for 56 assertion cases and four matrix scenarios. They exercise
equality, containment, JSON/schema/negation, Unicode, zero weights and thresholds,
numeric boundaries, overflow, explicit rejections, missing outputs, provider
errors, and a two-candidate × four-variable-combination × two-repetition matrix.
Tests compare through the public `Compile` and `Evaluate` interfaces.

```sh
go test -race ./internal/eval -run Evaluation.*Conformance
```

Ordinary `make verify` runs this committed corpus without additional source
checkouts or JavaScript tooling. `make test-eval` also requires real Postgres,
ClickHouse and isolated OCI scorer execution. Fixed synthetic fixtures establish
the declared contract; real model quality and serving cost require separate
qualification. Distribution terms are recorded in the repository notices.
