# Datasets for gateway testing

`test/datasets/tasks.jsonl` (32 rows, hand-written) is the seed set the k6
accuracy suite and `TestFixtureAccuracy` run against.

`tools/datasets/fetch.py` grows it with small slices of public datasets mapped
onto Conduit task classes. It tests **routing** (classification, constraint
safety, cost/quality trade-offs), never model weights.

## Secrets

| Variable | Where to get it | Needed for |
|---|---|---|
| `HF_TOKEN` | https://huggingface.co/settings/tokens | Higher rate limits; gated sets (GSM8K needs a token + license accept) |
| `KAGGLE_USERNAME` / `KAGGLE_KEY` | https://www.kaggle.com/settings/account | Kaggle sources (or `~/.kaggle/kaggle.json`) |
| `OPENROUTER_API_KEY` | https://openrouter.ai/keys | Reserved for future judge-labeled rows |

Local use: `cp .env.example .env` and fill in values (`.env` is git-ignored).

CI use: add the same names as **repository secrets**
(Settings → Secrets and variables → Actions). All three are optional — the
script skips sources whose creds are absent and still writes what it can.
`extended.jsonl` is git-ignored when fetched in CI; download it from the
workflow artifact instead.

## Run

```bash
pip install -r tools/datasets/requirements.txt
python3 tools/datasets/fetch.py --out test/datasets --max-per-source 200
```

This writes `test/datasets/extended.jsonl` + `test/datasets/SOURCES.md`
(provenance, licenses, counts). Spot-check new rows before trusting F1 deltas;
fetched text is untrusted third-party data — it is test input only and is
never executed.
