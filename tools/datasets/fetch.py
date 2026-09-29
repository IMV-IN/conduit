#!/usr/bin/env python3
"""Fetch small, license-friendly samples from Hugging Face / Kaggle and map
them onto Conduit task classes for gateway testing.

Secrets (all optional; the script degrades gracefully without them):
  HF_TOKEN         https://huggingface.co/settings/tokens (raises rate limits,
                   unlocks gated sets like gsm8k after you accept the license)
  KAGGLE_USERNAME / KAGGLE_KEY   https://www.kaggle.com/settings/account
                   (or ~/.kaggle/kaggle.json).
                   KAGGLE_API_TOKEN is accepted as an alias for KAGGLE_KEY
                   when KAGGLE_USERNAME is also set.
  OPENROUTER_API_KEY  reserved for future judge-based labeling (unused today)

Usage:
  pip install -r tools/datasets/requirements.txt
  python3 tools/datasets/fetch.py --out test/datasets --max-per-source 200

Output:
  test/datasets/extended.jsonl   {task, prompt[, tools]} rows, deduped against
                                 test/datasets/tasks.jsonl
  test/datasets/SOURCES.md       provenance + licenses + row counts

Only datasets with permissive or research-friendly licenses are used, and only
tiny slices (a few hundred rows) so CI stays fast. Full training corpora are
explicitly out of scope: these rows test *routing*, not model weights.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
from pathlib import Path

SOURCES = [
    # (hf_id, conduit_task, prompt_builder, license_note)
    ("openai_humaneval", "code",
     lambda r: f"Write a Python function for this task:\n{r['prompt']}",
     "HumanEval (MIT) — code generation prompts (prompts only, no solutions stored)"),
    ("gsm8k", "reasoning_math",
     lambda r: f"Solve step by step: {r['question']}",
     "GSM8K (MIT, gated — needs HF_TOKEN + license accept) — grade-school math"),
    ("samsum", "summarization",
     lambda r: f"Summarize this dialogue in two sentences:\n{r['dialogue']}",
     "SAMSum (CC BY-NC-ND 4.0 — eval use only) — dialogue summarization"),
    ("glue", "classification_short",
     lambda r: f"Classify the sentiment as positive or negative. Reply with one word: '{r['sentence']}'",
     "GLUE/sst2 (CC BY 4.0 lineage) — sentiment classification"),
]

KAGGLE_SOURCES = [
    # (kaggle_ref, conduit_task, note) — attempted only when creds exist.
    ("rtatman/fraudulent-email-corpus", "classification_short",
     "Fraudulent email corpus (research use) — spam classification"),
]


def load_existing_keys(base: Path) -> set[str]:
    keys: set[str] = set()
    for name in ("tasks.jsonl", "extended.jsonl"):
        p = base / name
        if not p.exists():
            continue
        for line in p.read_text().splitlines():
            line = line.strip()
            if not line:
                continue
            try:
                row = json.loads(line)
                keys.add(hashlib.sha256(row.get("prompt", "").encode()).hexdigest())
            except json.JSONDecodeError:
                continue
    return keys


def fetch_hf(dataset_id: str, limit: int):
    try:
        from datasets import load_dataset
    except ImportError:
        print("  [skip] `datasets` not installed (pip install -r tools/datasets/requirements.txt)",
              file=sys.stderr)
        return []
    token = os.environ.get("HF_TOKEN") or None
    try:
        if dataset_id == "glue":
            ds = load_dataset(dataset_id, "sst2", split=f"validation[:{limit}]", token=token)
        elif dataset_id == "gsm8k":
            ds = load_dataset(dataset_id, "main", split=f"test[:{limit}]", token=token)
        elif dataset_id == "samsum":
            ds = load_dataset(dataset_id, split=f"test[:{limit}]", token=token)
        else:
            ds = load_dataset(dataset_id, split=f"test[:{limit}]", token=token)
        return list(ds)
    except Exception as e:  # gated, offline, renamed — never fatal
        print(f"  [skip] {dataset_id}: {e}", file=sys.stderr)
        return []


def kaggle_creds():
    """Resolve (username, key) from env, `~/.kaggle/kaggle.json`, or the
    single-token form (KAGGLE_API_TOKEN as key + KAGGLE_USERNAME)."""
    user = os.environ.get("KAGGLE_USERNAME")
    key = os.environ.get("KAGGLE_KEY") or os.environ.get("KAGGLE_API_TOKEN")
    if user and key:
        return user, key
    kj = Path.home().joinpath(".kaggle/kaggle.json")
    if kj.exists():
        try:
            d = json.loads(kj.read_text())
            if d.get("username") and d.get("key"):
                return d["username"], d["key"]
        except (json.JSONDecodeError, OSError):
            pass
    return None, None


def fetch_kaggle(ref: str, limit: int):
    user, key = kaggle_creds()
    if not (user and key):
        print(f"  [skip] kaggle:{ref} (need KAGGLE_USERNAME + KAGGLE_KEY, "
              f"or KAGGLE_API_TOKEN + KAGGLE_USERNAME)", file=sys.stderr)
        return []
    try:
        from kaggle.api.kaggle_api_extended import KaggleApi
    except ImportError:
        print("  [skip] `kaggle` not installed", file=sys.stderr)
        return []
    # The kaggle package authenticates from KAGGLE_USERNAME/KAGGLE_KEY env
    # (or kaggle.json); export the resolved pair so the alias form works too.
    os.environ.setdefault("KAGGLE_USERNAME", user)
    if not os.environ.get("KAGGLE_KEY"):
        os.environ["KAGGLE_KEY"] = key
    try:
        api = KaggleApi()
        api.authenticate()
        tmp = Path("/tmp/opencode") / "kaggle" / ref.replace("/", "_")
        tmp.mkdir(parents=True, exist_ok=True)
        api.dataset_download_files(ref, path=str(tmp), unzip=True, quiet=True)
        files = sorted(tmp.glob("*.csv")) + sorted(tmp.glob("*.json"))
        if not files:
            return []
        import csv
        rows = []
        with open(files[0], newline="", encoding="utf-8", errors="replace") as f:
            for i, row in enumerate(csv.DictReader(f)):
                if i >= limit:
                    break
                rows.append(row)
        return rows
    except Exception as e:
        print(f"  [skip] kaggle:{ref}: {e}", file=sys.stderr)
        return []


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="test/datasets")
    ap.add_argument("--max-per-source", type=int, default=200)
    ap.add_argument("--seed", type=int, default=7)
    args = ap.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    seen = load_existing_keys(out)
    collected: list[dict] = []
    provenance: list[str] = []

    for hf_id, task, build, note in SOURCES:
        print(f"HF {hf_id} -> {task} ...")
        rows = fetch_hf(hf_id, args.max_per_source)
        kept = 0
        for r in rows:
            try:
                prompt = build(r)
            except (KeyError, TypeError):
                continue
            if not prompt or len(prompt) < 20:
                continue
            h = hashlib.sha256(prompt.encode()).hexdigest()
            if h in seen:
                continue
            seen.add(h)
            row: dict = {"task": task, "prompt": prompt[:4000]}
            if task == "classification_short":
                pass
            collected.append(row)
            kept += 1
        provenance.append(f"- {hf_id} ({note}): {kept} rows")
        print(f"  kept {kept}")

    for ref, task, note in KAGGLE_SOURCES:
        print(f"Kaggle {ref} -> {task} ...")
        rows = fetch_kaggle(ref, args.max_per_source)
        kept = 0
        for r in rows:
            text = r.get("text") or r.get("email") or r.get("Body") or next(iter(r.values()), "")
            text = str(text)[:2000]
            if len(text) < 20:
                continue
            h = hashlib.sha256(text.encode()).hexdigest()
            if h in seen:
                continue
            seen.add(h)
            collected.append({"task": task,
                              "prompt": f"Is this email spam or not spam? Reply with one word: '{text}'"})
            kept += 1
        provenance.append(f"- kaggle:{ref} ({note}): {kept} rows")
        print(f"  kept {kept}")

    ext = out / "extended.jsonl"
    with open(ext, "w") as f:
        for row in collected:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")
    (out / "SOURCES.md").write_text(
        "# Dataset provenance\n\nFetched "
        + __import__("datetime").date.today().isoformat()
        + " with `python3 tools/datasets/fetch.py`.\n\n" + "\n".join(provenance)
        + f"\n\nTotal: {len(collected)} rows in extended.jsonl (+ 32 curated in tasks.jsonl).\n"
        + "\nSeed set `tasks.jsonl` is hand-written; `extended.jsonl` is machine-fetched and "
        + "should be spot-checked before trusting classifier F1 deltas.\n"
    )
    print(f"\nwrote {ext} ({len(collected)} rows)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
