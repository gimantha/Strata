# Public memory benchmark

This folder measures how well a memory system helps a model answer questions about long
past conversations. It runs Strata and simple reference systems on the same public
questions, in the same way, and reports numbers anyone can check and run again.

It replaces the old `comparison/` harness. That one used our own questions, gave Strata
time filters and extra input fields that the other system did not get, and was tuned on
the same questions it was scored on. These rules exist to avoid all three.

## The rules

1. **Same questions.** Two public datasets, pinned to an exact version and checked by
   SHA-256 before every run:
   - **LoCoMo**: 10 long two-person chats, 1,540 questions (types 1–4; type 5 has no gold
     answer and is left out, as in other published results).
   - **LongMemEval-S**, cleaned release: 500 questions, each with about 115k tokens of chat
     history.
2. **Same input.** Every system gets the same sessions, speakers and session dates, and
   nothing else. Each system takes them in through its own best input path. No system
   gets the gold answer, the question type, or a time filter.
3. **Same answer step.** One answer prompt for everyone ([prompts.py](prompts.py)), one
   answer model, and one context budget: 4,000 tokens by default, counted with one
   tokenizer (`o200k_base`). The harness cuts any longer context to the budget. Only the
   `full-context` reference may go over, and the report says so.
4. **Same judge.** The judge prompts are copied word for word from published sources:
   the Mem0 paper's judge for LoCoMo (what most published LoCoMo scores use) and the
   official LongMemEval judge. One judge model for every system.
5. **Same models inside the systems.** When a system needs a model to read text or to
   make embeddings, every system uses the same one (`--extraction`, `--embedding`).
6. **Each system at its best.** Use each system's own recommended search mode and
   settings, and write the choice down in its adapter. A weak setup for a rival makes
   the result worthless.
7. **Tune only on dev.** Dev questions: LoCoMo `conv-26` and `conv-30`; for LongMemEval,
   the first 5 questions of each type in a fixed hash order. The headline number is the
   test split. If anyone looked at test failures while changing Strata, the report must
   say so.
8. **Report everything.** Accuracy with a 95% range, the paired difference between
   systems on the same questions, failures counted as wrong, evidence recall, tokens,
   speed, ingest cost, the exact settings and the Strata git commit.
9. **Run more than once before claiming a win.** In the old harness the same system moved
   5–10 points between two runs. If the range of a difference crosses zero, it is a tie.

## Systems

| Name | What it is | Status |
|---|---|---|
| `strata` | This repository. A fresh binary is built from the checkout for each run, with its own database | ready |
| `bm25` | Reference: plain keyword search over single turns, filled up to the budget | ready |
| `full-context` | Reference: the whole chat, no budget. Shows the answer model's ceiling. Costly on LongMemEval | ready |

Other memory systems are not part of this harness for now. The work here is to make
Strata better against these fixed references.

Strata reads conversations in its chat shape (`{"messages": [...]}`). That shape keeps the
speaker and session time on every turn. Strata can only call models through an
OpenAI-style API today, so `--extraction` and `--embedding` take `openai:<model>`. Claude
models need a small bridge, which is not built yet.

## How to run

Setup, once:

```bash
uv venv --python 3.12 bench/.venv
uv pip install --python bench/.venv/bin/python -r bench/requirements.lock
cp bench/.env.example bench/.env        # then add your keys; the file is git-ignored
bench/.venv/bin/python bench/run.py fetch locomo
```

You also need Docker (the harness starts its own PostgreSQL container,
`strata-bench-postgres`, on port 55441) and Go.

A run has four steps. Each step skips work it has already done, so you can stop it and
start it again at any time.

```bash
R=locomo-2026-11-06

# 1. Take in the chats and get a context for each question. Once per system.
bench/.venv/bin/python bench/run.py retrieve --run $R --dataset locomo --backend strata \
    --extraction openai:gpt-4.1-mini --embedding openai:text-embedding-3-small
bench/.venv/bin/python bench/run.py retrieve --run $R --dataset locomo --backend bm25

# 2. Answer every question from its context, with the same model for every system.
bench/.venv/bin/python bench/run.py answer --run $R --answerer anthropic:claude-haiku-5-5

# 3. Judge every answer.
bench/.venv/bin/python bench/run.py judge --run $R --judge anthropic:claude-sonnet-5-5

# 4. Write runs/$R/REPORT.md and runs/$R/scores.json.
bench/.venv/bin/python bench/run.py report --run $R
```

For a cheap first try, add `--conversations 1 --max-questions 20` to the retrieve step.
For LongMemEval, pick a fixed sample with `--per-category 10`.

Without any model keys, steps 1 and 4 still work with `--extraction none --embedding
hashing` (the defaults). The report then shows evidence recall, tokens and speed, but no
accuracy.

All systems in one run must use the same questions and budget. The harness refuses to mix
them, and refuses to mix answer or judge models inside one run.

Tests for the harness itself (offline, no keys):

```bash
bench/.venv/bin/python -m unittest discover -s bench -v
```

## What a run leaves behind

```
bench/runs/<run>/
  manifest.json        dataset and its SHA-256, the selection, the budget
  REPORT.md            the human report
  scores.json          the same numbers for tools
  <system>/
    backend.json       settings, Strata commit, ingest time and model cost
    contexts.jsonl     the context each question got, as the answer model saw it
    answers.jsonl      the full answer and the short final answer
    judgments.jsonl    each judge reply and its label
```

`bench/runs/` is git-ignored. Publish a finished run on purpose, with its whole folder.

## Cleaning up

Each Strata run keeps its own database so it can be resumed. To remove all of them:

```bash
docker rm -f strata-bench-postgres
```

## Sources and licenses

- LoCoMo data: [snap-research/locomo](https://github.com/snap-research/locomo), commit
  `3eb6f2c`. Shared for non-commercial use; it is downloaded, never committed.
- LongMemEval data: [xiaowu0162/longmemeval-cleaned](https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned),
  revision `98d7416`. MIT.
- LoCoMo judge prompt: [mem0ai/mem0](https://github.com/mem0ai/mem0) `evaluation/metrics/llm_judge.py`
  at commit `aae5989`. Apache-2.0.
- LongMemEval judge prompts: [xiaowu0162/LongMemEval](https://github.com/xiaowu0162/LongMemEval)
  `src/evaluation/evaluate_qa.py`. MIT.
