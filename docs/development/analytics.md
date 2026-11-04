# ClickHouse analytics projection

Postgres remains the authoritative store for branch events, evaluation completion,
and reports. `hypercall-projector` runs independently of the API and controller;
ClickHouse failure never blocks run admission or authoritative report retrieval.
The API's projection status reports pending count, oldest pending age in seconds,
and last delivered timestamp for the authenticated tenant/project. A running or
expired claim still counts as pending until its delivery is acknowledged.

## Configure and run

Use the pinned ClickHouse image
`clickhouse/clickhouse-server:25.8.3.66-alpine@sha256:707dcb39a8314977492b9b17a718f2aefb7ce9f27b8098c8b703d2a9f919007b`.
Create a database and install [the schema](../../deploy/clickhouse/schema.sql)
using an administrator credential. Give the worker account INSERT and SELECT
access to `hypercall_events` in that database. Supply credentials through the
process environment or the deployment's secret injection, without embedding them
in URLs or command arguments:

```sh
export HYPERCALL_POSTGRES_DSN='<Postgres DSN>'
export HYPERCALL_CLICKHOUSE_URL='https://clickhouse.example'
export HYPERCALL_CLICKHOUSE_DATABASE='hypercall'
export HYPERCALL_CLICKHOUSE_USER='projector'
export HYPERCALL_CLICKHOUSE_PASSWORD='<secret>'
go run ./cmd/hypercall-projector
```

The worker applies checked Postgres migrations on startup. ClickHouse schema is
installed separately so runtime credentials need no DDL privilege. HTTPS and
credentials are required. Local testing may opt into HTTP with
`--allow-insecure-loopback`; the hostname must be an explicit loopback IP.
Redirects are rejected, requests and response sizes are bounded, and HTTP errors
exclude server response bodies that might contain event data.

To drain the available backlog and exit, add `--drain`. It returns an error on a
failed sink request. Normal service mode retries failed delivery with bounded
backoff and logs the consumer, error, and next retry interval. Stop it with SIGTERM
or SIGINT. A canceled delivery may keep its claim for at most the 30-second lease;
a replacement worker can then retry it.

To inspect scoped lag without ClickHouse access or credentials:

```sh
go run ./cmd/hypercall-projector --status-tenant tenant-id --status-project project-id
```

## Delivery, queries, and recovery

The `clickhouse-v1` consumer independently discovers retained `events` and
`eval_outbox` rows. It never consumes or resets the lifecycle outbox's published
bit. Receipts are keyed by consumer, tenant, project, and source-prefixed event ID.
There is no increasing cursor: a transaction that commits late remains eligible.
The envelope retains tenant/project, event ID, resource ID, kind, timestamp,
version, and the JSON payload. All values are JSON encoded; query scopes use
ClickHouse query parameters.

A sink insertion is acknowledged in Postgres only after HTTP execution succeeds.
A committed insertion with a lost response is retried. Physical duplicate rows
are expected. `ReplacingMergeTree` orders by tenant, project, and immutable event
ID; **every logical analytics query must use `FINAL`** to deduplicate before
background merges. The package's scoped `Events` query does this. Raw `count(*)`
without `FINAL` measures delivery attempts, not event count.

When ClickHouse returns, the worker catches up from pending receipts. Verify the
scoped backlog reaches zero and inspect last delivery time. If the table was lost,
reinstall the schema, then replay all retained authoritative sources:

```sh
go run ./cmd/hypercall-projector --rebuild --drain
```

Rebuild deletes only this consumer's receipts. It never truncates ClickHouse,
changes another consumer, or changes lifecycle/report authority. An in-flight
worker's old token cannot acknowledge a replacement receipt. Running workers may
continue during rebuild; duplicate insertions are safe. Replay covers retained
Postgres source rows only: deleting authoritative history removes that recovery
source. Do not drop retained source rows before the required recovery window.

## Real local acceptance

Install the schema in the selected test database before testing. Set
`HYPERCALL_TEST_POSTGRES`, `HYPERCALL_TEST_CLICKHOUSE_URL`,
`HYPERCALL_TEST_CLICKHOUSE_DATABASE`, `HYPERCALL_TEST_CLICKHOUSE_USER`, and
`HYPERCALL_TEST_CLICKHOUSE_PASSWORD`, then run:

```sh
go test -race ./internal/analytics
```

Without these variables the integrations explicitly skip. The tests use separate
Postgres schemas and unique tenant IDs in the shared ClickHouse test database.
They exercise real ClickHouse insertion and FINAL deduplication, scoped reads and
lag, HTTP outage with an available authoritative report, committed insertion with
an ambiguous response, restart catchup, consumer-independent branch events,
rebuild replay, concurrent claim exclusion, and stale acknowledgment fencing.
These are local projection correctness checks; they establish no production
throughput, retention capacity, or ClickHouse availability commitment.
