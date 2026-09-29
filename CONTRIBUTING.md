# Contributing to Conduit

## PR checklist

1. `make test-short` passes; new routing/math code adds unit or property tests.
2. `go vet ./...` clean; `staticcheck` / `govulncheck` clean in CI.
3. `docs/DESIGN.md` updated if you changed behavior, metrics, or ledger fields.
4. `conduit.example.yaml` + JSON-schema-relevant validation updated if config changed.
5. k6 impact noted: does this touch the hot path? If yes, run
   `RATE=300 DUR=15s make k6-overhead` and paste `test-results/overhead.json`.
6. Failure mode documented: what happens when your new dependency fails?

## Definition of done (per feature, from IMPLEMENTATION_PLAN §5)

Unit + property tests; race clean; metrics + ledger fields; schema + example
config; benchmarks unchanged or better; failure mode documented.

## Secrets

Never commit tokens. Use `.env` locally (see `.env.example`) and repository
secrets in CI. Fetched dataset rows are untrusted input — spot-check them.
