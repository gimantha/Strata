"""Deterministic, synthetic event stream. Answer keys never enter either graph."""

from __future__ import annotations

import hashlib
import json
import random
from pathlib import Path

ROOT = Path(__file__).resolve().parent
SEED = 20260911


def build(projects: int = 100):
    rng = random.Random(SEED)
    cases = []
    for i in range(1, projects + 1):
        tags = rng.sample(
            ["Alder", "Birch", "Cedar", "Elm", "Fir", "Maple", "Oak", "Pine"], 3
        )
        cases.append(
            dict(
                project=f"Project-{i:03d}",
                a=f"{tags[0]}-{i:03d}",
                b=f"{tags[1]}-{i:03d}",
                c=f"{tags[2]}-{i:03d}",
                first=f"Mara-{i:03d}",
                second=f"Theo-{i:03d}",
                budget=10000 + rng.randrange(10, 90) * 100,
                increase=rng.randrange(10, 50) * 100,
            )
        )
    events = []
    by_case = {}
    world_days = [1, 3, 5, 8, 10, 12, 3, 25, 9, 29]
    arrival_days = [1, 3, 5, 8, 10, 12, 22, 25, 28, 29]
    for wave in range(1, 11):
        order = cases[:]
        rng.shuffle(order)
        for c in order:
            p, a, b, final = c["project"], c["a"], c["b"], c["c"]
            first, second, budget = c["first"], c["second"], c["budget"]
            new_budget = budget + c["increase"]
            event_id = f"EV-{len(events)+1:04d}"
            old = by_case.setdefault(p, {})
            old[wave] = event_id
            bodies = {
                1: f"{p} is opened. Its accountable owner is {first}, effective 2026-01-01. Its approved budget is USD {budget}, effective 2026-01-01. {a} is only a proposed supplier; no supplier has been approved yet.",
                2: f"Security review for {p}: supplier {a} failed its security audit on 2026-01-03. The review records reason SECURITY_AUDIT_FAILURE: missing access-control evidence. This report is believed correct when received.",
                3: f"Decision for {p}: {first} approves supplier {b}, effective 2026-01-05. The stated reason is SECURITY_AUDIT_FAILURE at {a}, reported by {old.get(2, '')}, and {b}'s valid security certification. The decision relies on that audit report. This is an actual approval, replacing the earlier proposal of {a}.",
                4: f"Routine progress for {p}: supplier {b} delivered a prototype on 2026-01-08. Its checksum is CHECK-{p[-3:]}-A. This delivery changes neither approved budget nor ownership nor supplier selection.",
                5: f"Budget amendment for {p}, effective 2026-01-10: the approved budget is now USD {new_budget}, replacing USD {budget}. Reason SCOPE_EXPANSION: an additional integration was approved. {first} authorizes this amendment.",
                6: f"Ownership transfer for {p}, effective 2026-01-12: {second} becomes the sole accountable owner, replacing {first}. {first}'s ownership was valid from 2026-01-01 until, but excluding, 2026-01-12. The approved supplier and budget are unchanged.",
                7: f"Correction received 2026-01-22 for {p}: audit report {old.get(2, '')} was a FALSE_POSITIVE. {a} actually passed its security audit on 2026-01-03; the access-control evidence had been filed under the wrong identifier. This corrects what happened on January 3. It does not change the historical fact that {first} chose {b} on January 5 because the audit was then believed to have failed. It does not itself change the approved supplier.",
                8: f"Replacement decision for {p}, effective 2026-01-25: {second} cancels the selection of {b} and approves {final} as the sole supplier. Reason SERVICE_OUTAGE: {b} cannot meet the deployment date after an outage. {b}'s selection was valid from 2026-01-05 until, but excluding, 2026-01-25. The approved budget remains USD {new_budget}.",
                9: f"Late delivery for {p}: this record was written on 2026-01-09 but first received on 2026-01-28. It says {first} was the accountable owner as of January 9. This describes the historical interval ending on 2026-01-12, not a new appointment. Its source version precedes the transfer recorded by {old.get(6, '')}.",
                10: f"Temporary operating instruction for {p}: the active incident contact is {first} from 2026-01-29T00:00:00Z until 2026-01-30T00:00:00Z. This temporary contact expires at that time and is distinct from accountable owner {second}. No successor incident contact is specified. Supplier {final} remains selected.",
            }
            received = f"2026-01-{arrival_days[wave-1]:02d}T12:00:00Z"
            happened = f"2026-01-{world_days[wave-1]:02d}T00:00:00Z"
            text = f"Event ID: {event_id}\nProject: {p}\nSource: procurement-register\nReceived at: {received}\nWorld event time: {happened}\n\n{bodies[wave]}"
            events.append(
                dict(
                    event_id=event_id,
                    project=p,
                    wave=wave,
                    arrival_index=len(events) + 1,
                    received_at=received,
                    event_time=happened,
                    source_sequence=str(45 if wave == 9 else wave * 10),
                    source_version=f"v{45 if wave == 9 else wave*10}",
                    operation="correction" if wave == 7 else "append",
                    content=text,
                )
            )

    # Selection is fixed before either engine runs. Ten independent projects at full scale.
    selected = cases[:: max(1, projects // 10)][:10]
    questions = []

    def q(c, suffix, category, text, expected, waves, checkpoint=1000, **extra):
        questions.append(
            dict(
                id=f"{c['project']}-{suffix}-{checkpoint}",
                project=c["project"],
                category=category,
                checkpoint=checkpoint,
                question=f"For {c['project']}, {text}",
                expected={
                    k: v if isinstance(v, list) else [str(v)]
                    for k, v in expected.items()
                },
                evidence_ids=[by_case[c["project"]][w] for w in waves],
                **extra,
            )
        )

    for selected_index, c in enumerate(selected):
        end = projects * 10
        p, a, b, final = c["project"], c["a"], c["b"], c["c"]
        # The cohort is chosen before either engine runs. It supplies a stable regression
        # holdout without changing the event distribution or hiding any answer key.
        holdout = selected_index % 5 == 4
        before = len(questions)
        q(
            c,
            "past-supplier",
            "temporal",
            "which supplier was selected on 2026-01-07, based on all records now available?",
            {"supplier": b},
            [3, 8],
            end,
            valid_at="2026-01-07T12:00:00Z",
        )
        q(
            c,
            "past-owner",
            "temporal",
            "who was accountable owner on 2026-01-09, based on all records now available?",
            {"owner": c["first"]},
            [6, 9],
            end,
            valid_at="2026-01-09T12:00:00Z",
        )
        q(
            c,
            "known-before",
            "temporal",
            "using only information received by 2026-01-12, was the January 3 audit of "
            + a
            + " believed passed or failed?",
            {"audit_status": "failed"},
            [2],
            end,
            known_checkpoint=projects * 6,
        )
        q(
            c,
            "known-after",
            "temporal",
            "using all information received by 2026-01-29, did "
            + a
            + " actually pass or fail its January 3 audit?",
            {"audit_status": "passed"},
            [7],
            end,
        )
        q(
            c,
            "budget-delta",
            "reasoning",
            "by how many USD did the approved budget increase on January 10?",
            {"increase_usd": c["increase"]},
            [1, 5],
            end,
        )
        q(
            c,
            "cause-chain",
            "decision_rationale",
            "why did the January 5 approver select "
            + b
            + "? Return the recorded reason code and approver, even if evidence was corrected later.",
            {"reason_code": "SECURITY_AUDIT_FAILURE", "approver": c["first"]},
            [2, 3, 7],
            end,
        )
        q(
            c,
            "switch-cause",
            "decision_rationale",
            "why was the supplier changed on January 25, and who authorized it? Return the recorded reason code and approver.",
            {"reason_code": "SERVICE_OUTAGE", "approver": c["second"]},
            [8],
            end,
        )
        q(
            c,
            "current-budget",
            "updates",
            "what is the current approved budget in USD as of January 29?",
            {"budget_usd": c["budget"] + c["increase"]},
            [5],
            end,
        )
        q(
            c,
            "late-owner",
            "updates",
            "who is the accountable owner as of January 29, after the late January 9 record was received?",
            {"owner": c["second"]},
            [6, 9],
            end,
        )
        q(
            c,
            "current-supplier",
            "invalidation",
            "which sole supplier is currently selected as of January 29?",
            {"supplier": final},
            [8],
            end,
        )
        q(
            c,
            "expired-contact",
            "invalidation",
            "who is the active incident contact at 2026-02-01T00:00:00Z? If none is specified after expiry, return contact=unknown.",
            {"contact": ["unknown", "none", "not specified"]},
            [10],
            end,
        )
        q(
            c,
            "approval-source",
            "provenance",
            "what event ID records the January 5 supplier approval, and who approved it?",
            {"approval_event": by_case[p][3], "approver": c["first"]},
            [3],
            end,
        )
        # Queries during ingestion show actual pre-update state, not only retrospective answers.
        q(
            c,
            "live-supplier",
            "updates",
            "which supplier is approved using only events received so far?",
            {"supplier": b},
            [3],
            projects * 3,
        )
        q(
            c,
            "live-owner",
            "updates",
            "who is now accountable owner using only events received so far?",
            {"owner": c["second"]},
            [6],
            projects * 6,
        )
        for question in questions[before:]:
            question["cohort"] = "holdout" if holdout else "development"
    return events, questions


def write_dataset(projects=100):
    folder = ROOT / "fixtures"
    folder.mkdir(exist_ok=True)
    events, questions = build(projects)
    for name, rows in (("events.jsonl", events), ("questions.jsonl", questions)):
        (folder / name).write_text(
            "".join(json.dumps(r, sort_keys=True) + "\n" for r in rows)
        )
    manifest = dict(
        seed=SEED,
        projects=projects,
        events=len(events),
        questions=len(questions),
        checkpoints=sorted({q["checkpoint"] for q in questions}),
        sha256={
            n: hashlib.sha256((folder / n).read_bytes()).hexdigest()
            for n in ["events.jsonl", "questions.jsonl"]
        },
    )
    (folder / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    return manifest


if __name__ == "__main__":
    print(json.dumps(write_dataset(), indent=2))
