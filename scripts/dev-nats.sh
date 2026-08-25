#!/usr/bin/env bash
# Starts or stops a local NATS server with JetStream, for distributed-mode tests.
#
# Port 14222 rather than the default 4222: a developer's own NATS server should not have this
# project's streams created in it, and a test suite that adopts whatever is on the default
# port is a surprise waiting to happen.
#
#   scripts/dev-nats.sh start   # boot a server and print its URL
#   scripts/dev-nats.sh stop    # shut it down
#   scripts/dev-nats.sh status  # report whether it is running
set -euo pipefail

PORT=${NATS_PORT:-14222}
CONTAINER=${STRATA_NATS_CONTAINER:-strata-nats}
IMAGE=${STRATA_NATS_IMAGE:-nats:2.10-alpine}

case "${1:-start}" in
start)
    command -v docker >/dev/null 2>&1 || {
        echo "docker is required; install it or set TEST_NATS_URL to an existing server" >&2
        exit 1
    }
    if [[ -z "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        if [[ -n "$(docker ps -aq -f "name=^${CONTAINER}$")" ]]; then
            docker start "$CONTAINER" >/dev/null
        else
            holder=$(docker ps --format '{{.Names}} {{.Ports}}' | awk -v p=":$PORT->" '$0 ~ p {print $1}')
            if [[ -n "$holder" ]]; then
                echo "port $PORT is already published by container $holder." >&2
                echo "Use it directly, remove it, or set NATS_PORT." >&2
                exit 1
            fi
            # -js enables JetStream. Without it the server accepts connections and fails
            # every stream operation, which is the confusing failure this avoids.
            docker run -d --name "$CONTAINER" -p "$PORT:4222" "$IMAGE" -js >/dev/null
        fi
    fi
    for _ in $(seq 1 30); do
        docker exec "$CONTAINER" nats-server --help >/dev/null 2>&1 && break
        sleep 1
    done
    echo "NATS with JetStream is ready on port $PORT."
    echo "  export TEST_NATS_URL=\"nats://127.0.0.1:$PORT\"   # only if you changed the port"
    echo
    echo "The test harness finds this server automatically. Set CG_REQUIRE_NATS=1 to turn a"
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
