#!/usr/bin/env bash
# Verifies the package rule from AGENTS.md section 5: internal/domain must not depend on
# database, HTTP, LLM, embedding, message-bus, telemetry, or provider packages.
#
# The rule exists so the canonical model can outlive any particular backend. A guard is
# needed because the violation is one careless import away and nothing else would fail.
set -euo pipefail

MODULE=$(go list -m)
ALLOWED_EXTERNAL="github.com/google/uuid"

violations=$(go list -deps ./internal/domain | while read -r dep; do
    # Standard library packages have no dot in their first path element.
    first_element=${dep%%/*}
    if [[ "$first_element" != *.* ]]; then
        continue
    fi
    # Our own domain package and its allowed pure-library dependencies are fine.
    if [[ "$dep" == "$MODULE/internal/domain" ]]; then
        continue
    fi
    for allowed in $ALLOWED_EXTERNAL; do
        if [[ "$dep" == "$allowed" || "$dep" == "$allowed"/* ]]; then
            continue 2
        fi
    done
    echo "$dep"
done)

if [[ -n "$violations" ]]; then
    echo "internal/domain must stay free of infrastructure dependencies, but it imports:" >&2
    echo "$violations" | sed 's/^/  /' >&2
    echo >&2
    echo "Move the dependency behind a port owned by the service that needs it." >&2
    exit 1
fi

echo "internal/domain is free of infrastructure dependencies"
