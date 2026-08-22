#!/usr/bin/env bash
# Starts, stops, or resets a local PostgreSQL cluster for development.
#
# No Docker required: this uses the PostgreSQL server binaries directly, the same way the
# integration test harness does when TEST_DATABASE_URL is unset.
#
#   scripts/dev-postgres.sh start   # boot a cluster and print its connection URL
#   scripts/dev-postgres.sh stop    # shut it down
#   scripts/dev-postgres.sh reset   # drop and recreate the strata database
#   scripts/dev-postgres.sh psql    # open a shell against it
set -euo pipefail

PGPORT=${PGPORT:-55432}
DATA_ROOT=${STRATA_PGROOT:-.tmp/postgres}
DATA_DIR="$DATA_ROOT/data"
LOG_FILE="$DATA_ROOT/server.log"
DB_NAME=${STRATA_DB:-strata}
DSN="postgres://postgres@127.0.0.1:$PGPORT/$DB_NAME"

find_bindir() {
    if [[ -n "${PG_BINDIR:-}" ]]; then
        echo "$PG_BINDIR"
        return
    fi
    if command -v initdb >/dev/null 2>&1; then
        dirname "$(command -v initdb)"
        return
    fi
    # Debian and Ubuntu keep server binaries out of PATH.
    local newest
    newest=$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)
    if [[ -n "$newest" ]]; then
        echo "$newest"
        return
    fi
    echo "no PostgreSQL server binaries found; set PG_BINDIR" >&2
    exit 1
}

BINDIR=$(find_bindir)

# PostgreSQL refuses to run as root, so drop to a service account when necessary.
as_postgres() {
    if [[ "$(id -u)" == "0" ]]; then
        su "${PGUSER_ACCOUNT:-postgres}" -c "$1"
    else
        bash -c "$1"
    fi
}

case "${1:-start}" in
start)
    mkdir -p "$DATA_ROOT"
    if [[ "$(id -u)" == "0" ]]; then
        chown -R "${PGUSER_ACCOUNT:-postgres}" "$DATA_ROOT"
    fi
    if [[ ! -d "$DATA_DIR" ]]; then
        as_postgres "$BINDIR/initdb -D $(realpath "$DATA_DIR" 2>/dev/null || echo "$DATA_DIR") -A trust -U postgres" >/dev/null
    fi
    if ! "$BINDIR/pg_isready" -h 127.0.0.1 -p "$PGPORT" >/dev/null 2>&1; then
        as_postgres "$BINDIR/pg_ctl -D $DATA_DIR -l $LOG_FILE -o '-p $PGPORT -c listen_addresses=127.0.0.1 -c fsync=off' -w start"
    fi
    psql "postgres://postgres@127.0.0.1:$PGPORT/postgres" -tAc \
        "SELECT 1 FROM pg_database WHERE datname='$DB_NAME'" | grep -q 1 ||
        psql "postgres://postgres@127.0.0.1:$PGPORT/postgres" -qc "CREATE DATABASE $DB_NAME"
    echo "PostgreSQL is ready."
    echo "  export CG_DATABASE_URL=\"$DSN\""
    echo "  export TEST_DATABASE_URL=\"postgres://postgres@127.0.0.1:$PGPORT/postgres\""
    ;;
stop)
    as_postgres "$BINDIR/pg_ctl -D $DATA_DIR -m fast -w stop" || true
    ;;
reset)
    psql "postgres://postgres@127.0.0.1:$PGPORT/postgres" -qc "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)"
    psql "postgres://postgres@127.0.0.1:$PGPORT/postgres" -qc "CREATE DATABASE $DB_NAME"
    echo "database $DB_NAME recreated"
    ;;
psql)
    exec psql "$DSN"
    ;;
*)
    echo "usage: $0 {start|stop|reset|psql}" >&2
    exit 2
    ;;
esac
