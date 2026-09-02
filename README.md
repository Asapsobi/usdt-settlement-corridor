# USDT Settlement Corridor

A specialised USDT cross-network settlement layer — BEP20 → TRC20 — sold not as a
swap service but as **TRC20 payout infrastructure for businesses**.

> *"Reliable TRC20 payouts, funded from any chain."*

This repository is the working record of the evaluation, the architecture decisions
that came out of it, and the build specifications derived from those decisions —
and, as of 1 Sep 2026, the ledger core itself.

## Where things stand

| | |
|---|---|
| Market verdict | Not viable as "network conversion"; viable as payout infrastructure |
| Recommended model | **Model D** — conversion/payout API now; Model E (netting) year 2–3 |
| Margin engine | Wholesale TRON energy (25.7 sun blend vs 41 sun market) + batch multisend |
| Contribution margin | 87.1% at a $3,000 ticket |
| MVP scope | 6 services, one ledger, no smart contracts · ≈9–10 eng-weeks |
| Build status | **C1 (ledger core) built and tested** — all chunks C1.0–C1.11 shipped, C1.9 replay gate passing at 10,000 orders / 32 workers. Scenario catalog audited row-by-row against the real test suite (3 coverage gaps found and closed, one real HTTP-boundary bug found and fixed). C1.11 added the reversal/reorg HTTP surface (`POST /v1/entries/{id}/reversal`, `POST /v1/orders/{id}/reorg`) that C2 needs and C1.8 hadn't exposed. See `docs/03-build/c1-scenario-catalog.md`. |
| Next action | **C2 — deposit watcher (BSC)** is specified (`docs/03-build/c2-deposit-watcher-build-prompts.md`), not yet built. Of its 4 prerequisites: the C1 reorg-endpoint gap is now closed (C1.11); a deposit-sweep entry type/owner and a policy for deposits landing after quote expiry are still open, but neither blocks starting C2.0; BSC finality timing has been re-derived (block time ~0.45s post-Fermi hard fork) and should be verified against your own RPC providers before it's trusted for customer-facing SLA copy. On the business side, the week-2 wholesale pricing calls to Tronsell/Netts flagged in the findings doc — not confirmed done as of this write-up. |

## Documents

### `docs/01-strategy/`

- **[findings-and-recommendation.md](docs/01-strategy/findings-and-recommendation.md)** —
  the market assessment. Verdict, the five findings that drive everything, structural
  market facts, pricing recommendation, expected 24-month outcome, and the single gate
  question that decides whether the business continues. Includes two addenda: the
  "competitors do it free" objection checked against real pricing, and a survey of
  TRON energy rental partners.

### `docs/02-architecture/`

- **[product-operations-architecture.md](docs/02-architecture/product-operations-architecture.md)** —
  eleven architecture and operations decisions with the numbers behind each: rent vs
  stake energy, segregated payout slots vs a pooled treasury, the three service tiers,
  treasury rebalancing, the capital ladder, and why the status page is the primary
  sales asset. Includes the economics summary, GTM sequence, and the assumption test
  schedule.
- **[component-map.md](docs/02-architecture/component-map.md)** —
  decomposition into six services (C1–C6) plus three supporting pieces, with the
  dependency graph, per-component ownership boundaries, hard parts, and what is
  deliberately *not* built at MVP.

### `docs/03-build/`

- **[c1-ledger-build-prompts.md](docs/03-build/c1-ledger-build-prompts.md)** —
  the ledger core, specified as eleven sequenced build chunks (C1.0 → C1.10) with
  acceptance criteria for each, written to be handed to an AI coding agent one chunk
  at a time. Includes the chart of accounts (§A) and the worked double-entry
  conversion example (§B) that the whole design rests on. **Built, plus C1.11** —
  the reversal/reorg HTTP surface C2 needs, added after C2's own spec surfaced that
  C1.8 hadn't exposed it — see `ledger/` and `ledger/docs/errors.md`.
- **[c1-scenario-catalog.md](docs/03-build/c1-scenario-catalog.md)** —
  the full scenario / risk catalog for the corridor: what's engineered and gated in
  C1 today, what cross-component failure modes are still open (C2–C6, not yet built),
  and what's irreducible risk that has to be priced or insured rather than fixed.
  Audited against the real test suite on 1 Sep 2026.
- **[c2-deposit-watcher-build-prompts.md](docs/03-build/c2-deposit-watcher-build-prompts.md)** —
  the deposit watcher, specified the same way C1 was: sequenced build chunks
  (C2.0 → C2.10) with acceptance criteria, written for an AI coding agent. Opened
  with four prerequisite gaps against the already-built C1; the endpoint gap is
  now closed (C1.11) and the spec updated to match the shipped shape. Still open:
  a deposit-sweep entry type/owner and a product decision for deposits arriving
  after quote expiry (neither blocks starting), plus a re-derivation of BSC's
  current finality timing worth verifying against real RPC providers before it's
  trusted for SLA copy. Not yet built.

### `ledger/`

The built C1 service — Go + PostgreSQL, no ORM, hand-written SQL, `pgx/v5`, `chi`
routing, `goose` migrations. `cmd/ledgerd` (the service), `cmd/migrate`, `cmd/replay`
(the C1.9 ship-gate harness), `cmd/seed-console` (seeds accounts for the manual test
console at `docs/console.html`). Packages under `internal/`: `money`, `accounts`,
`journal`, `orders`, `recon`, `halt`, `httpapi`, `replay`. Operational docs live at
`ledger/docs/`: `runbook.md`, `accounts.md`, `errors.md`, `openapi.yaml`.

## Reading order

If you are new to this: **findings → architecture decisions → component map → C1 build
prompts → scenario catalog → `ledger/`.** Each document assumes the previous one is
settled and does not re-open it.

## Repository conventions

- Decisions live in documents, not in commit messages or chat history. If a decision
  changed, the document changes and the diff is the record.
- Numbers carry their baseline. Anything derived from TRX at $0.34, 74,750 blended
  energy per payout, or $255k of working float says so.
- A document does not re-litigate a decision made upstream of it.
- Now that code exists, **the repository is the canonical source of build status.**
  The project workspace this repo mirrors from can go stale between visits — it only
  updates when someone explicitly checks git — so when the two disagree on what's
  built, trust this repo.

## Status of the numbers

Every figure here is either observed, modelled, or assumed — and the documents mark
which. The assumptions carrying the most weight, and the dates by which they must be
tested, are tabulated in the architecture decisions record under *"What must be
tested, and by when."* Treat untested assumptions as untested.

---

Private working repository. Not an offer, not investment advice, and not a
description of a live service.
