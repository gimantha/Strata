#!/usr/bin/env bash
set -euo pipefail
comparison_root="$(cd "$(dirname "$0")" && pwd)"
cd "$comparison_root/.."
uv venv --allow-existing comparison/.venv --python python3
uv pip install --python comparison/.venv/bin/python -r comparison/requirements.lock
mkdir -p comparison/.runtime
go build -o comparison/.runtime/contextgraphd ./cmd/contextgraphd
if ! docker inspect strata-comparison-postgres >/dev/null 2>&1; then
    docker run -d --name strata-comparison-postgres \
        -e POSTGRES_PASSWORD=comparison-local -e POSTGRES_DB=strata_comparison \
        -p 127.0.0.1:55439:5432 pgvector/pgvector:pg16
else
    docker start strata-comparison-postgres >/dev/null
fi
for _ in $(seq 1 60); do
    if docker exec strata-comparison-postgres pg_isready -U postgres >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
docker exec strata-comparison-postgres pg_isready -U postgres
comparison/.venv/bin/python comparison/dataset.py
comparison/.venv/bin/python -m unittest discover -s comparison -p 'test_*.py'
