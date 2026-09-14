"""Apply deterministic release thresholds to a predeclared comparison cohort."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def evaluate(rows, system, cohort="holdout", minimum_grounded=0.97):
    selected = [
        row
        for row in rows
        if row.get("system") == system and row.get("cohort") == cohort
    ]
    errors = sum(bool(row.get("error")) for row in selected)
    grounded = [row.get("score", {}).get("grounded_accuracy", 0) for row in selected]
    citations = [row.get("score", {}).get("citation_validity", 0) for row in selected]
    grounded_rate = sum(grounded) / len(selected) if selected else 0
    citation_rate = sum(citations) / len(selected) if selected else 0
    passed = bool(selected) and errors == 0 and grounded_rate >= minimum_grounded and citation_rate == 1
    return dict(
        system=system,
        cohort=cohort,
        questions=len(selected),
        errors=errors,
        grounded_accuracy=round(grounded_rate, 4),
        citation_validity=round(citation_rate, 4),
        minimum_grounded=minimum_grounded,
        passed=passed,
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("answers", type=Path)
    parser.add_argument("--system", default="strata")
    parser.add_argument("--cohort", default="holdout")
    parser.add_argument("--minimum-grounded", type=float, default=0.97)
    args = parser.parse_args()
    rows = [json.loads(line) for line in args.answers.read_text().splitlines() if line.strip()]
    result = evaluate(rows, args.system, args.cohort, args.minimum_grounded)
    print(json.dumps(result, indent=2))
    raise SystemExit(0 if result["passed"] else 1)


if __name__ == "__main__":
    main()
