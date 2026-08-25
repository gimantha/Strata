#!/usr/bin/env bash
# Proves a backup can be restored and the derived indexes rebuilt from it
# (AGENTS.md section 40: "Regularly test restore + projection rebuild").
#
# A backup nobody has restored is a hypothesis. This runs the real sequence — dump, restore
# into a fresh database, drop every derived record, rebuild from the canonical ledger alone,
# and confirm retrieval still answers — so the hypothesis becomes a fact with a date on it.
#
#   CG_DRILL_WORKSPACE=acme scripts/restore-drill.sh
#   CG_DRILL_WORKSPACE=acme scripts/restore-drill.sh postgres://.../mydb
#
# Requires pg_dump, psql, and a PostgreSQL the current role may create databases in. Nothing
# is written to the source: the drill dumps it and works on the copy.
set -euo pipefail

SOURCE=${1:-${CG_DATABASE_URL:-}}
if [[ -z "$SOURCE" ]]; then
    echo "usage: $0 <source-dsn>   (or set CG_DATABASE_URL)" >&2
    exit 2
fi

for tool in pg_dump psql go; do
    command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 2; }
done

WORKSPACE=${CG_DRILL_WORKSPACE:-}
if [[ -z "$WORKSPACE" ]]; then
    echo "set CG_DRILL_WORKSPACE to the workspace slug or id to verify" >&2
    exit 2
fi

STAMP=$(date +%Y%m%d%H%M%S)
RESTORED="strata_drill_${STAMP}"
DUMP=$(mktemp -t strata-drill-XXXXXX.dump)
# The admin connection is the source with its database swapped for the maintenance one, so
# the drill needs no second credential.
ADMIN=$(printf '%s' "$SOURCE" | sed -E 's#(/)[^/?]+(\?|$)#\1postgres\2#')

cleanup() {
    rm -f "$DUMP"
    if [[ "${CG_DRILL_KEEP:-0}" != "1" ]]; then
        psql "$ADMIN" -q -c "DROP DATABASE IF EXISTS ${RESTORED} WITH (FORCE)" >/dev/null 2>&1 || true
    else
        echo "keeping the restored copy: ${RESTORED}"
    fi
}
trap cleanup EXIT

echo "1/5  dumping the source"
pg_dump --format=custom --file="$DUMP" "$SOURCE"
echo "     $(du -h "$DUMP" | cut -f1) written"

echo "2/5  restoring into ${RESTORED}"
psql "$ADMIN" -q -c "CREATE DATABASE ${RESTORED}"
TARGET=$(printf '%s' "$SOURCE" | sed -E "s#(/)[^/?]+(\?|\$)#\1${RESTORED}\2#")
# --no-owner, because the drill restores as whoever is running it rather than as the
# production role, and a drill that needs production credentials will not be run.
pg_restore --no-owner --dbname="$TARGET" "$DUMP"

echo "3/4  checking the restore covers every table the schema has"
CG_DATABASE_URL="$TARGET" CG_AUTO_MIGRATE=false \
    go run ./cmd/cgctl recovery classify >/dev/null

echo "4/4  dropping every derived record and rebuilding from the canonical ledger"
CG_DATABASE_URL="$TARGET" CG_AUTO_MIGRATE=false \
    go run ./cmd/cgctl recovery drill --workspace "$WORKSPACE" --confirm

echo
echo "Drill passed against a restored copy. Record the date: a backup nobody has restored"
echo "is a hypothesis, and this is what turns it into a fact."
