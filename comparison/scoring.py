"""Transparent scoring: exact structured facts and verifiable source excerpts.

No engine's output is used to write the answer key. No LLM judges correctness.
"""

import re
from collections import defaultdict


def normalize(value):
    text = str(value).strip().casefold()
    if re.fullmatch(r"[\d,]+(?:\.0+)?", text):
        return str(int(float(text.replace(",", ""))))
    return re.sub(r"\s+", " ", text).strip(" .\"'")


def score(question, answer, context, events):
    facts = answer.get("facts", [])
    # Duplicated keys with contradictory values fail that field, not last-write-wins.
    proposed = defaultdict(set)
    for f in facts:
        if isinstance(f, dict):
            proposed[f.get("key", "")].add(normalize(f.get("value", "")))
    fields = {}
    for key, accepted in question["expected"].items():
        fields[key] = bool(proposed[key]) and proposed[key] <= {
            normalize(v) for v in accepted
        }
    correct = sum(fields.values()) / len(fields)
    cited, verified = set(), set()
    citations = answer.get("citations", [])
    checks = []
    for item in citations:
        if not isinstance(item, dict):
            continue
        eid, quote = item.get("event_id", ""), item.get("quote", "")
        cited.add(eid)
        # A real quote must be in BOTH the cited source and the supplied retrieval context.
        # Header-only quotes are excluded: they prove identity, not the stated facts.
        source = events.get(eid, {}).get("content", "").split("\n\n", 1)[-1]
        valid = (
            isinstance(quote, str)
            and len(quote.strip()) >= 25
            and quote in source
            and quote in context
        )
        if valid:
            verified.add(eid)
        checks.append(dict(event_id=eid, verified=valid))
    targets = set(question["evidence_ids"])
    recall = len(verified & targets) / len(targets) if targets else 1.0
    precision = sum(c["verified"] for c in checks) / len(checks) if checks else 0.0
    visible = set(re.findall(r"\bEV-\d{4}\b", context))
    return dict(
        fact_accuracy=correct,
        all_facts_correct=all(fields.values()),
        fields=fields,
        evidence_recall=recall,
        citation_validity=precision,
        evidence_id_retrieval_recall=len(targets & visible) / len(targets),
        grounded_accuracy=correct * recall,
        citation_checks=checks,
    )


def aggregate(rows):
    groups = defaultdict(list)
    for row in rows:
        groups[(row["system"], row["category"])].append(row)
    output = {}
    for (system, category), items in groups.items():
        # Failed queries stay in the denominator and receive zero.
        output.setdefault(system, {})[category] = dict(
            questions=len(items),
            errors=sum(bool(i.get("error")) for i in items),
            **{
                metric: round(
                    100
                    * sum(i.get("score", {}).get(metric, 0) for i in items)
                    / len(items),
                    2,
                )
                for metric in [
                    "fact_accuracy",
                    "evidence_recall",
                    "citation_validity",
                    "grounded_accuracy",
                ]
            }
        )
    for system, categories in output.items():
        categories["macro"] = {
            metric: round(
                sum(c[metric] for c in categories.values()) / len(categories), 2
            )
            for metric in [
                "fact_accuracy",
                "evidence_recall",
                "citation_validity",
                "grounded_accuracy",
            ]
        }
    return output
