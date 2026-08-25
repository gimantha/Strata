#!/usr/bin/env bash
# Starts or stops a local MinIO server, for object-store tests.
#
# Port 19000 rather than the default 9000: a developer's own MinIO should not have this
# project's buckets created and dropped in it, and a test suite that adopts whatever is on
# the default port is a surprise waiting to happen.
#
#   scripts/dev-minio.sh start   # boot a server and print its endpoint
#   scripts/dev-minio.sh stop    # shut it down
#   scripts/dev-minio.sh status  # report whether it is running
set -euo pipefail

PORT=${MINIO_PORT:-19000}
CONTAINER=${STRATA_MINIO_CONTAINER:-strata-minio}
IMAGE=${STRATA_MINIO_IMAGE:-minio/minio:latest}
# Test credentials for a local container, which is why they are in a script rather than a
# secret manager. Override them for anything reachable from outside this machine.
ACCESS_KEY=${MINIO_ACCESS_KEY:-strata}
SECRET_KEY=${MINIO_SECRET_KEY:-strata-secret}

case "${1:-start}" in
start)
    command -v docker >/dev/null 2>&1 || {
        echo "docker is required; install it or set TEST_S3_ENDPOINT to an existing server" >&2
        exit 2
    }
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "already running on port $PORT"
        exit 0
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$CONTAINER" \
        -p "${PORT}:9000" \
        -e "MINIO_ROOT_USER=${ACCESS_KEY}" \
        -e "MINIO_ROOT_PASSWORD=${SECRET_KEY}" \
        "$IMAGE" server /data >/dev/null

    for _ in $(seq 1 30); do
        if (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null; then
            break
        fi
        sleep 1
    done
    echo "MinIO is ready on port $PORT."
    echo "  export TEST_S3_ENDPOINT=\"http://127.0.0.1:${PORT}\"   # only if you changed the port"
    echo
    echo "The test harness finds this server automatically. Set CG_REQUIRE_S3=1 to turn a"
    echo "missing server into a failure rather than a skip."
    ;;
stop)
    docker stop "$CONTAINER" >/dev/null 2>&1 || true
    ;;
status)
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "running on port $PORT"
    else
        echo "not running"
    fi
    ;;
*)
    echo "usage: $0 {start|stop|status}" >&2
    exit 2
    ;;
esac
