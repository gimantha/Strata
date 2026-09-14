"""Verify real local initialization without extraction, embeddings, or benchmark scores."""

import asyncio
import time

from adapters import Cognee, Strata
from common import ROOT, now, redact_error, save
from run import MODELS


async def main():
    run_id = "infrastructure-" + time.strftime("%Y%m%d-%H%M%S")
    results = dict(
        run_id=run_id,
        started_at=now(),
        model_calls=0,
        benchmark_events_ingested=0,
        systems={},
    )
    for cls in [Strata, Cognee]:
        engine = cls(
            run_id,
            ROOT / ".runtime" / run_id / cls.name,
            "preflight-not-a-model-key",
            MODELS,
        )
        try:
            await engine.start()
            if cls is Strata:
                folder = ROOT / ".runtime" / run_id / "snapshots"
                folder.mkdir(parents=True, exist_ok=True)
                stats = await engine.snapshot(folder, 0, [])
                assert stats["assertions"] == 0 and stats["entities"] == 0
                results["systems"][cls.name] = dict(
                    status="passed",
                    checks=[
                        "compiled server starts",
                        "migrations/readiness",
                        "API-key authentication",
                        "workspace/graph-space/source creation",
                        "scoped empty canonical snapshot",
                    ],
                )
            else:
                results["systems"][cls.name] = dict(
                    status="passed",
                    checks=[
                        "pinned package imports",
                        "SDK configuration",
                        "release model and backend defaults resolved",
                        "logs/data paths isolated",
                    ],
                )
        except Exception as exc:
            results["systems"][cls.name] = dict(
                status="failed", error=redact_error(exc)
            )
            raise
        finally:
            await engine.close()
            save(ROOT / "results/infrastructure-check.json", results)
    results["ended_at"] = now()
    save(ROOT / "results/infrastructure-check.json", results)
    print("Infrastructure checks passed. Zero benchmark events and zero model calls.")


if __name__ == "__main__":
    asyncio.run(main())
