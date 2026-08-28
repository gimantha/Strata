#!/usr/bin/env bash
# Starts or stops a local Neo4j, for the graph-backend tests.
#
# Ports 17687 (Bolt) and 17474 (HTTP) rather than the defaults: a developer's own server
# should not have this project's nodes created and deleted in it. The adapter speaks Bolt.
#
#   scripts/dev-neo4j.sh start|stop|status
set -euo pipefail

BOLT_PORT=${NEO4J_BOLT_PORT:-17687}
HTTP_PORT=${NEO4J_HTTP_PORT:-17474}
CONTAINER=${STRATA_NEO4J_CONTAINER:-strata-neo4j}
IMAGE=${STRATA_NEO4J_IMAGE:-neo4j:5-community}
# Test credentials for a local container, which is why they are in a script rather than a
# secret manager. Override for anything reachable from outside this machine.
PASSWORD=${NEO4J_PASSWORD:-strata-secret}

case "${1:-start}" in
start)
    command -v docker >/dev/null 2>&1 || {
        echo "docker is required; install it or set TEST_NEO4J_URI" >&2
        exit 2
    }
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "already running on Bolt ${BOLT_PORT}"
        exit 0
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker run -d --name "$CONTAINER" \
        -p "${BOLT_PORT}:7687" -p "${HTTP_PORT}:7474" \
        -e "NEO4J_AUTH=neo4j/${PASSWORD}" \
        -e NEO4J_server_memory_heap_max__size=512m \
        -e NEO4J_server_memory_pagecache_size=256m \
        "$IMAGE" >/dev/null

    # A JVM, so the wait is generous.
    for _ in $(seq 1 60); do
        if curl -s -m 3 -o /dev/null "http://127.0.0.1:${HTTP_PORT}/" 2>/dev/null; then
            echo "Neo4j is ready: Bolt ${BOLT_PORT}, HTTP ${HTTP_PORT}."
            echo
            echo "The test harness finds it automatically. Set CG_REQUIRE_NEO4J=1 to turn a"
            echo "missing server into a failure rather than a skip."
            exit 0
        fi
        sleep 2
    done
    echo "Neo4j never became ready" >&2
    docker logs "$CONTAINER" 2>&1 | tail -20 >&2
    exit 1
    ;;
stop)
    docker stop "$CONTAINER" >/dev/null 2>&1 || true
    ;;
status)
    if [[ -n "$(docker ps -q -f "name=^${CONTAINER}$")" ]]; then
        echo "running on Bolt ${BOLT_PORT}"
    else
        echo "not running"
    fi
    ;;
*)
    echo "usage: $0 {start|stop|status}" >&2
    exit 2
    ;;
esac
