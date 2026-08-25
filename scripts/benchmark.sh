#!/usr/bin/env bash
# Runs the performance benchmarks from AGENTS.md section 39 and prints each result with the
# conditions it was measured under.
#
# Not part of `go test ./...`: these load a corpus and take minutes, and a benchmark that
# runs on every commit becomes a benchmark everyone skips. The three invariants section 39
# also asks for — bounded traversal, bounded context, no full scan — are ordinary tests and
# do run in CI, because those either hold or the system is wrong.
#
#   scripts/benchmark.sh              # every benchmark
#   scripts/benchmark.sh Retrieval    # only those matching a pattern
#
# Targets are configurable, so a deployment can hold itself to its own numbers:
#   CG_BENCH_RETRIEVAL_P95=250ms CG_BENCH_PROJECTION_LAG_P95=2s scripts/benchmark.sh
set -euo pipefail

PATTERN=${1:-.}
ITERATIONS=${CG_BENCH_ITERATIONS:-200}

# A real database, never a mock: these measure storage behaviour, and a fake would report
# whatever its author expected. CG_REQUIRE_PG turns a missing server into a failure so a
# benchmark run cannot silently measure nothing.
export CG_REQUIRE_PG=1

echo "Running section 39 benchmarks (${ITERATIONS} iterations each)."
echo "Every result prints its dataset size, hardware, index configuration, embedding model,"
echo "and query mix, because a number without its conditions is not a measurement."
echo

go test ./internal/benchmarks/ \
    -bench "$PATTERN" \
    -benchtime "${ITERATIONS}x" \
    -run '^$' \
    -timeout 60m \
    -v
