#!/usr/bin/env bash
# Starts or stops a local OpenSearch, for the lexical-backend tests.
#
# Port 19200 rather than 9200: a developer's own cluster should not have this project's
# indices created and dropped in it, which is the same reasoning the PostgreSQL, NATS, MinIO
# and Qdrant harnesses use.
#
# Single node, security plugin off, small heap. This is a test fixture, not a deployment
# template — a real cluster wants authentication and rather more memory.
#
#   scripts/dev-opensearch.sh start|stop|status
set -euo pipefail

PORT=${OPENSEARCH_PORT:-19200}
CONTAINER=${STRATA_OPENSEARCH_CONTAINER:-strata-opensearch}
IMAGE=${STRATA_OPENSEARCH_IMAGE:-opensearchproject/opensearch:2}
HEAP=${OPENSEARCH_HEAP:-512m}

case "${1:-start}" in
start)
    command -v docker >/dev/null 2>&1 || {
        echo "docker is required; install it or set TEST_OPENSEARCH_URL" >&2
        exit 2
    }
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "already running on port $PORT"
        exit 0
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$CONTAINER" \
        -p "${PORT}:9200" \
        -e discovery.type=single-node \
        -e DISABLE_SECURITY_PLUGIN=true \
        -e DISABLE_INSTALL_DEMO_CONFIG=true \
        -e "OPENSEARCH_JAVA_OPTS=-Xms${HEAP} -Xmx${HEAP}" \
        "$IMAGE" >/dev/null

    # A JVM takes longer to start than the other services, so the wait is generous.
    for _ in $(seq 1 60); do
        if [[ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/" || true)" == "200" ]]; then
            echo "OpenSearch is ready on port ${PORT}."
            echo
            echo "The test harness finds it automatically. Set CG_REQUIRE_OPENSEARCH=1 to turn"
            echo "a missing cluster into a failure rather than a skip."
            exit 0
        fi
        sleep 2
    done
    echo "OpenSearch never became ready" >&2
    docker logs "$CONTAINER" 2>&1 | tail -20 >&2
    exit 1
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
