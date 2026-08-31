# USDT Settlement Corridor

A specialised USDT cross-network settlement layer — BEP20 → TRC20 — sold not as a
swap service but as **TRC20 payout infrastructure for businesses**.

> *"Reliable TRC20 payouts, funded from any chain."*

This repository is the working record of the evaluation, the architecture decisions
that came out of it, and the build specifications derived from those decisions.
It is documentation-first by design: nothing is coded until the decision that
justifies it is written down and reviewed here.

## Where things stand

| | |
|---|---|
| Market verdict | Not viable as "network conversion"; viable as payout infrastructure |
| Recommended model | **Model D** — conversion/payout API now; Model E (netting) year 2–3 |
| Margin engine | Wholesale TRON energy (25.7 sun blend vs 41 sun market) + batch multisend |
| Contribution margin | 87.1% at a $3,000 ticket |
| MVP scope | 6 services, one ledger, no smart contracts · ≈9–10 eng-weeks |
| Build status | C1 (ledger core) specified, not yet started |
| Next action | Week-2 wholesale pricing calls to Tronsell and Netts |

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
  conversion example (§B) that the whole design rests on.

## Reading order

If you are new to this: **findings → architecture decisions → component map → C1 build
prompts.** Each document assumes the previous one is settled and does not re-open it.

## Repository conventions

- Decisions live in documents, not in commit messages or chat history. If a decision
  changed, the document changes and the diff is the record.
- Numbers carry their baseline. Anything derived from TRX at $0.34, 74,750 blended
  energy per payout, or $255k of working float says so.
- A document does not re-litigate a decision made upstream of it.

## Status of the numbers

Every figure here is either observed, modelled, or assumed — and the documents mark
which. The assumptions carrying the most weight, and the dates by which they must be
tested, are tabulated in the architecture decisions record under *"What must be
tested, and by when."* Treat untested assumptions as untested.

---

Private working repository. Not an offer, not investment advice, and not a
description of a live service.
