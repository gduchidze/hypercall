#!/bin/sh
# Run with PostgreSQL 17 client tools. The caller provisions an empty, isolated
# destination database; this script refuses a destination containing user tables.
set -eu
: "${HYPERCALL_DATABASE_URL:?source database URL is required}"
: "${HYPERCALL_RESTORE_DATABASE_URL:?isolated empty destination URL is required}"
: "${HYPERCALL_BACKUP_FILE:?absolute backup output path is required}"
case "$HYPERCALL_BACKUP_FILE" in /*) ;; *) echo 'backup path must be absolute' >&2; exit 1;; esac
test "$HYPERCALL_DATABASE_URL" != "$HYPERCALL_RESTORE_DATABASE_URL" || { echo 'source and destination must differ' >&2; exit 1; }
case "$(pg_dump --version)" in *' 17.'*) ;; *) echo 'PostgreSQL 17 client tools required' >&2; exit 1;; esac
count=$(psql "$HYPERCALL_RESTORE_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c "SELECT count(*) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog','information_schema')")
test "$count" = 0 || { echo 'restore destination is not empty; refusing to overwrite it' >&2; exit 1; }
# Stop API/controller/hosts before restoring an execution database. DB epochs
# alone cannot revoke capabilities held by hosts running against an older backup.
pg_dump "$HYPERCALL_DATABASE_URL" --schema=public --format=custom --no-owner --no-privileges --file="$HYPERCALL_BACKUP_FILE"
pg_restore --dbname="$HYPERCALL_RESTORE_DATABASE_URL" --clean --if-exists --exit-on-error --single-transaction --no-owner --no-privileges "$HYPERCALL_BACKUP_FILE"
source_rows=$(mktemp)
restored_rows=$(mktemp)
trap 'rm -f "$source_rows" "$restored_rows"' EXIT HUP INT TERM
schema_version=$(psql "$HYPERCALL_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c 'SELECT COALESCE(max(version),0) FROM public.schema_migrations')
for table in projects resources idempotency branches jobs events outbox schema_migrations agent_calls agent_steps; do
 case "$table" in
  agent_calls|agent_steps)
   source_exists=$(psql "$HYPERCALL_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c "SELECT to_regclass('public.$table') IS NOT NULL")
   restored_exists=$(psql "$HYPERCALL_RESTORE_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c "SELECT to_regclass('public.$table') IS NOT NULL")
   test "$source_exists" = "$restored_exists" || { echo "restored $table presence differs" >&2; exit 1; }
   if test "$source_exists" = f; then
    test "$schema_version" -lt 4 || { echo "schema 4 requires $table; refusing incomplete verification" >&2; exit 1; }
    printf '%s|absent-before-schema-4\n' "$table" >> "$source_rows"
    printf '%s|absent-before-schema-4\n' "$table" >> "$restored_rows"
    continue
   fi
   ;;
 esac
 query="SELECT '$table',count(*),md5(COALESCE(string_agg(to_jsonb(t)::text,E'\\n' ORDER BY to_jsonb(t)::text),'')) FROM public.$table t"
 psql "$HYPERCALL_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c "$query" >> "$source_rows"
 psql "$HYPERCALL_RESTORE_DATABASE_URL" -X -At -v ON_ERROR_STOP=1 -c "$query" >> "$restored_rows"
done
cmp "$source_rows" "$restored_rows"
cat "$restored_rows"
echo 'restore row fingerprints match; API inspection and migration re-run remain required'
