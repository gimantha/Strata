#!/usr/bin/env bash
# Starts or stops a local Qdrant, for the vector-backend tests.
#
# Ports 16333 (REST) and 16334 (gRPC) rather than the defaults: a developer's own Qdrant
# should not have this project's collections created and dropped in it. The adapter speaks
# gRPC, so 16334 is the one the tests use.
#
#   scripts/dev-qdrant.sh start   # boot a server
#   scripts/dev-qdrant.sh stop    # shut it down
#   scripts/dev-qdrant.sh status  # report whether it is running
set -euo pipefail

REST_PORT=${QDRANT_REST_PORT:-16333}
GRPC_PORT=${QDRANT_GRPC_PORT:-16334}
CONTAINER=${STRATA_QDRANT_CONTAINER:-strata-qdrant}
IMAGE=${STRATA_QDRANT_IMAGE:-qdrant/qdrant:latest}

case "${1:-start}" in
start)
    command -v docker >/dev/null 2>&1 || {
        echo "docker is required; install it or set TEST_QDRANT_HOST to an existing server" >&2
        exit 2
    }
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "already running on ports ${REST_PORT}/${GRPC_PORT}"
        exit 0
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$CONTAINER" \
        -p "${REST_PORT}:6333" -p "${GRPC_PORT}:6334" "$IMAGE" >/dev/null

    for _ in $(seq 1 30); do
        if (exec 3<>"/dev/tcp/127.0.0.1/${GRPC_PORT}") 2>/dev/null; then
            break
        fi
        sleep 1
    done
    echo "Qdrant is ready: REST ${REST_PORT}, gRPC ${GRPC_PORT}."
    echo
    echo "The test harness finds this server automatically. Set CG_REQUIRE_QDRANT=1 to turn"
    echo "a missing server into a failure rather than a skip."
    ;;
stop)
    docker stop "$CONTAINER" >/dev/null 2>&1 || true
    ;;
status)
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "running on ports ${REST_PORT}/${GRPC_PORT}"
    else
        echo "not running"
    fi
    ;;
*)
    echo "usage: $0 {start|stop|status}" >&2
    exit 2
    ;;
esac
