#!/usr/bin/env bash
# Starts, stops, or resets a local PostgreSQL cluster for development.
#
# Two backends, chosen automatically: the PostgreSQL server binaries when they exist (the
# same way the integration test harness works), and Docker when they do not. Either way the
# result listens on the same port, so nothing downstream has to know which one ran.
#
# pgvector is required, not optional: the projections migration creates the extension, so a
# server without it fails every integration test. A local installation provides it via
# "brew install pgvector" or "apt install postgresql-16-pgvector"; the Docker backend uses an
# image that ships with it.
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
CONTAINER=${STRATA_PG_CONTAINER:-strata-postgres}
IMAGE=${STRATA_PG_IMAGE:-pgvector/pgvector:pg16}

have_binaries() {
    [[ -n "${PG_BINDIR:-}" ]] && return 0
    command -v initdb >/dev/null 2>&1 && return 0
    compgen -G "/usr/lib/postgresql/*/bin" >/dev/null 2>&1 && return 0
    compgen -G "/opt/homebrew/opt/postgresql@*/bin" >/dev/null 2>&1 && return 0
    return 1
}

# Docker backend. Used only when no server binaries exist, so a machine with a real
# PostgreSQL installation never has a container started behind its back.
docker_backend() {
    command -v docker >/dev/null 2>&1 || {
        echo "no PostgreSQL server binaries and no docker; install one of them" >&2
        exit 1
    }
    case "${1:-start}" in
    start)
        if [[ -z "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
            if [[ -n "$(docker ps -aq -f "name=^${CONTAINER}$")" ]]; then
                docker start "$CONTAINER" >/dev/null
            else
                # Report a taken port as itself. Docker's own failure here names a
                # bind error, which sends people looking in the wrong place.
                holder=$(docker ps --format '{{.Names}} {{.Ports}}' | awk -v p=":$PGPORT->" '$0 ~ p {print $1}')
                if [[ -n "$holder" ]]; then
                    echo "port $PGPORT is already published by container $holder." >&2
                    echo "Use it directly, remove it, or set PGPORT to another port." >&2
                    exit 1
                fi
                docker run -d --name "$CONTAINER" \
                    -e POSTGRES_PASSWORD=postgres -p "$PGPORT:5432" "$IMAGE" >/dev/null
            fi
        fi
        # The container answers the port before the server accepts connections, so poll.
        for _ in $(seq 1 60); do
            docker exec "$CONTAINER" pg_isready -U postgres >/dev/null 2>&1 && break
            sleep 1
        done
        docker exec "$CONTAINER" psql -U postgres -tAc \
            "SELECT 1 FROM pg_database WHERE datname='$DB_NAME'" | grep -q 1 ||
            docker exec "$CONTAINER" psql -U postgres -qc "CREATE DATABASE $DB_NAME"
        echo "PostgreSQL is ready in container $CONTAINER ($IMAGE)."
        echo "  export CG_DATABASE_URL=\"postgres://postgres:postgres@127.0.0.1:$PGPORT/$DB_NAME?sslmode=disable\""
        echo
        echo "TEST_DATABASE_URL is not needed: the test harness finds this server on port $PGPORT."
        ;;
    stop)
        docker stop "$CONTAINER" >/dev/null 2>&1 || true
        ;;
    reset)
        docker exec "$CONTAINER" psql -U postgres -qc "DROP DATABASE IF EXISTS $DB_NAME WITH (FORCE)"
        docker exec "$CONTAINER" psql -U postgres -qc "CREATE DATABASE $DB_NAME"
        echo "database $DB_NAME recreated"
        ;;
    psql)
        exec docker exec -it "$CONTAINER" psql -U postgres "$DB_NAME"
        ;;
    *)
        echo "usage: $0 {start|stop|reset|psql}" >&2
        exit 2
        ;;
    esac
    exit 0
}

if ! have_binaries; then
    docker_backend "${1:-start}"
fi

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
