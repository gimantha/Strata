#!/usr/bin/env bash
# Starts or stops a local Ollama and pulls a model, for the live query-planner tests.
#
# The default port, unlike the other dev scripts: Ollama holds models rather than this
# project's data, so sharing a developer's own server costs nothing and re-downloading
# several gigabytes to avoid it costs a great deal.
#
# These tests are not part of the default gate. They need a model on the machine, CI has no
# GPU, and what they assert about a model's judgment is necessarily weaker than what the
# rest of the suite asserts. They exist because a scripted model answers the way its author
# expected, and a real one does not: the first reply this produced labelled four sub-queries
# "original", which no scripted test had thought to do.
#
#   scripts/dev-ollama.sh start    # start the server and pull the model
#   scripts/dev-ollama.sh stop     # shut it down
#   scripts/dev-ollama.sh status   # report whether it is running
#   scripts/dev-ollama.sh test     # run the live planner tests against it
set -euo pipefail

MODEL=${TEST_OLLAMA_MODEL:-qwen2.5:3b}
URL=${TEST_OLLAMA_URL:-http://127.0.0.1:11434}
LOG=${STRATA_OLLAMA_LOG:-/tmp/strata-ollama.log}

running() { curl -fsS -m 2 "${URL}/api/version" >/dev/null 2>&1; }

case "${1:-start}" in
start)
    command -v ollama >/dev/null 2>&1 || {
        echo "ollama is required: brew install ollama (or see https://ollama.com)" >&2
        echo "or set TEST_OLLAMA_URL to a server that already has ${MODEL}" >&2
        exit 2
    }
    if running; then
        echo "already running at ${URL}"
    else
        echo "starting ollama, logging to ${LOG}"
        nohup ollama serve >"${LOG}" 2>&1 &
        for _ in $(seq 1 60); do running && break; sleep 1; done
        running || { echo "ollama did not come up; see ${LOG}" >&2; exit 1; }
    fi
    # Roughly 2GB for the default. Pulling is idempotent.
    ollama pull "${MODEL}"
    echo "ready: ${MODEL} at ${URL}"
    ;;
stop)
    pkill -f "ollama serve" 2>/dev/null || true
    echo "stopped"
    ;;
status)
    if running; then
        echo "running at ${URL}"
        ollama list
    else
        echo "not running"
        exit 1
    fi
    ;;
test)
    running || { echo "not running; try: scripts/dev-ollama.sh start" >&2; exit 1; }
    CG_REQUIRE_OLLAMA=1 go test -count=1 -timeout 20m -v \
        ./internal/retrieval/ -run TestLiveModel
    ;;
*)
    echo "usage: $0 {start|stop|status|test}" >&2
    exit 2
    ;;
esac
