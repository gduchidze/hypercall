# Evaluation workers, reports and CI

Phase 3 evaluates recorded evidence. It does not dispatch a model matrix or
provision infrastructure. Whole-task evidence must originate from an initial
branch; continuation evidence must originate from one common retained checkpoint.
The supported output selector is `raw_utf8`: the response bytes of an explicitly
selected committed inference call. An assertion pass describes that response,
not completion of an arbitrary agent task. Fixture results and measurements are
synthetic, including fixture calls recorded through real branch APIs.

The [compatibility subset](evaluation-compatibility.md) and
[recorded acceptance](phase3-validation.md) describe the actual tested envelope.
Real inference, billing reconciliation and live judges remain Phase 4A work.
This stage does not qualify the outstanding Phase 1B fan-out/cross-host/GCS gate.

## Processes

Run the authenticated API as described in [control-plane.md](control-plane.md).
An evaluation worker is a separate role from the same Go codebase:

```sh
export HYPERCALL_DATABASE_URL='postgres://USER:PASSWORD@HOST/DATABASE?sslmode=require'
go run ./cmd/hypercall-evaluator
```

It applies checked migrations and consumes a durable queue independent of sandbox
jobs. `--drain` processes currently eligible evaluations then exits. Plain
deterministic assertions need no scorer configuration. Custom grading requires a
private `--scorers-file` containing an operator-controlled JSON array of pinned
`scorer.Config` records; see [scorers.md](scorers.md). `Timeout` is an integer
duration in nanoseconds. Each process generates a fresh scorer owner identity.
An unregistered scorer digest fails as an evaluation error.

Each evaluation has a persisted Operation and immutable plan/input digest. A
worker claims it with a fenced epoch and renewable 30-second lease. It performs
grading outside database transactions, then atomically publishes its immutable
report, completes the operation and appends the analytics outbox record. A lost
lease cannot publish. Failed/canceled worker attempts remain reclaimable; attempt
counts survive restarts. Reported scoring costs are unknown after a retry because
an abandoned attempt may have incurred additional cost. No model/tool mutation
is automatically replayed by this queue.

`policy.timeout_seconds` bounds a whole evaluation to 1..3600 seconds, default
300. OCI scorers also enforce individual CPU, memory, PID and deadline limits.
After confirming an old evaluator owner is fenced, an operator can clean its
abandoned containers using `--scorers-file FILE --recover-fenced-owner OLD_OWNER`.
An independent container watchdog bounds survival if its evaluator is killed.

Start the independent [ClickHouse projector](analytics.md) to consume durable
events and final reports. `analytics-status` and report responses expose pending
projection count and age from Postgres. A ClickHouse outage cannot erase a report
or turn an evaluation into a success. Projector receipts are independent of other
outbox consumers; duplicates and rebuilds deduplicate by scoped source event ID.

## Compile and submit

```yaml
apiVersion: hypercall.io/eval/v1
scope: whole_task
output_selector: raw_utf8
workload_digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
candidates:
  - id: candidate
    model_digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
cases:
  - id: greeting
    vars:
      name: [Alice, Bob]
    assert:
      - type: equals
        value: "Hello {{name}}"
repetitions: 2
policy:
  minimum_samples: 2
  minimum_pass_rate: 1
  timeout_seconds: 300
```

```sh
hypercall compile-eval evaluation.yaml > plan.json
hypercall --key experiment-001 evaluate evaluation.yaml observations.json
hypercall evaluation EVALUATION_ID
hypercall --format markdown report EVALUATION_ID
hypercall --check report EVALUATION_ID > report.json
hypercall analytics-status
```

The compiler returns stable cell IDs for the candidate × resolved case variables
× repetition matrix. Observations are a JSON array keyed by those IDs. Synthetic
evidence must say `source: synthetic`, with an explicit output (empty string is
valid), state and trace completeness. Optional synthetic costs use integer
microUSD, `kind: synthetic`, an immutable `price_digest` and RFC3339 `recorded_at`;
latency is an optional nonnegative integer `latency_ms`.

Branch-backed observations contain only `cell_id`, `source: branch`, `branch_id`
and `call_id`. The API resolves response bytes, model identity, adapter provenance
and finalization from the tenant-scoped committed ledger. Client-provided branch
output, completion or cost is rejected. One branch may populate only one cell;
repetitions require distinct executions. Branches must share fixed agent,
workspace, template, execution profile, command/environment/resources and tool
policy. Here `workload_digest` is the pinned Task dataset digest. Continuations
must share one retained parent checkpoint; a declared `shared_prefix.id` must
match it. Trace finalization does not claim sandbox cleanup has completed.

Missing observations produce retained nonpassing cells. Existing call records
do not contain trustworthy serving latency, token billing or model cost, so those
measurements remain unknown. Public prefix cost inputs are synthetic only. No
cost is inferred from a token count or replaced with zero.

Python sync and async clients expose `compile_evaluation`, `evaluate`,
`evaluation`, `report`, and `projection_status`. Mutations require an idempotency
key and are never automatically retried. Reusing the same original branch
references returns the first captured evaluation even if a trace later changes.
Changed input with the same key conflicts.

## Decisions and accounting

Reports retain all planned cells, errors, source/adapter provenance, assertion
results, candidate score means and pass rates, sample counts, and Wilson 95%
intervals. Missing/error cells stay in the pass-rate denominator; score means
use evaluated samples separately. Repetitions and intervals describe the declared
fixture/workload, not a general reliability guarantee.

The quality gate defaults to all cells passing. `minimum_pass_rate` can declare
a lower rate; execution/evaluation errors and incomplete matrices still fail.
`baseline_report_id` references an immutable report in the same tenant/project;
its scope, workload, cases, repetitions, cache policy, retained checkpoint and
resolved fixed-input provenance must match. Optional
`max_score_drop` permits a declared candidate mean-score drop (default zero).
Incompatible or incomplete baselines are rejected at admission.

Continuation shared-prefix cost is counted once for the experiment. Candidate
costs include suffix execution and grading. Missing prefix pricing makes the experiment total
unknown. Scoring cost and execution cost are separate report fields; configured
cost budgets include both. Unknown scoring/model/prefix costs fail a configured
budget as `measurement_missing`. Per-assertion scoring records retain their
currency, kind, source and error category. MicroUSD totals are exact integer
accounting; individual scorer USD amounts are rounded to microUSD.

Pareto membership requires complete, quality-qualified candidates with declared
minimum samples and known cost/latency. It compares candidates only within one
compiled workload and cache policy. Missing or errored candidates cannot become
winners. The Pareto report is synthetic until real measurement qualification.

`--check` exits nonzero for simultaneous machine-readable reasons including
`quality_regression`, `budget_exceeded`, `execution_error`, `evaluation_error`,
`incomplete_matrix`, `insufficient_samples`, `measurement_missing`, and malformed
measurement/overflow errors. Threshold zero and metric-only weight zero never
override execution, scorer, missing-result or aggregation errors.

Admission rejects unknown YAML fields, scripts, remote schemas, unsupported
expressions and unpinned inputs. Limits are 1 MiB source YAML, 1,000 cells,
128 candidates, 64 assertions per case, 4,000 resolved assertion applications,
2 MiB resolved matrix/input and depth 16. Plan plus retained baseline is limited
to 3 MiB. Assertion explanations and scorer cost-source strings share a 128 KiB
budget measured after JSON escaping; `reason_truncated`/`source_truncated` mark
omissions, and a source digest preserves cost provenance identity. Scores,
errors and amounts remain retained. These bounds keep admitted reports within
the public RPC size.
These limits
are admission limits, not capacity or latency guarantees.
