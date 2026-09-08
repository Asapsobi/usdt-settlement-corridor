# USDT Settlement Corridor

A specialised USDT cross-network settlement layer — BEP20 → TRC20 — sold not as a
swap service but as **TRC20 payout infrastructure for businesses**.

> *"Reliable TRC20 payouts, funded from any chain."*

This repository is the working record of the evaluation, the architecture decisions
that came out of it, and the build specifications derived from those decisions —
and, as of 7 Sep 2026, the ledger core, the deposit watcher, screening, and the
energy broker, plus a payout-dispatcher build spec with no code behind it yet.

## Where things stand

| | |
|---|---|
| Market verdict | Not viable as "network conversion"; viable as payout infrastructure |
| Recommended model | **Model D** — conversion/payout API now; Model E (netting) year 2–3 |
| Margin engine | Wholesale TRON energy (25.7 sun blend vs 41 sun market) + batch multisend |
| Contribution margin | 87.1% at a $3,000 ticket |
| MVP scope | 6 services, one ledger, no smart contracts · ≈9–10 eng-weeks |
| Build status | **C1 (ledger core), C2 (deposit watcher), C3 (screening), and C4 (energy broker) are built and tested; C5 (payout dispatcher) has a build spec but no code.** C1: all chunks C1.0–C1.11 shipped, C1.9 replay gate passing at 10,000 orders / 32 workers, scenario catalog audited row-by-row against the real test suite (3 coverage gaps found and closed, one real HTTP-boundary bug found and fixed). C1.11 added the reversal/reorg HTTP surface (`POST /v1/entries/{id}/reversal`, `POST /v1/orders/{id}/reorg`) that C2 needs. C2: all chunks C2.0–C2.10 shipped, including the replay harness and HTTP boundary, wired to a live chain-watching engine and verified against real BSC. C3: all chunks C3.0–C3.9 shipped, including its own replay ship-gate harness; `cmd/screend` now also runs the discovery background loop for real (against a real, running C1 — no fake, no vendor dependency). The pipeline and re-screen loops are still not wired: both need a real `provider.ScreeningProvider` (an AML vendor client), and vendor choice among the Chainalysis/TRM/Elliptic candidates `component-map.md` names was deliberately left unmade — wiring a mock into a production compliance path would silently screen real orders against a fake verdict, which is worse than the honest gap. C4: all chunks C4.0–C4.9 shipped, including its own replay ship-gate harness (9/9 scenarios, 6/6 final assertions passing against a real ledger and real Postgres); `cmd/brokerd` is now fully wired to production, including real Tronsell/Netts/CatFee HTTP integrations and a real TronGrid on-chain reader, added after discovering none of the three vendors' real APIs support the delegation-retargeting the original buffer design assumed — see the "Read this fourth" addendum in `docs/03-build/c4-energy-broker-build-prompts.md`. Going live still needs the real payout slot addresses, a confirmed Tronsell base URL, and a whitelisted Netts egress IP — all operator-supplied, none fabricated. C5: build prompts only (`docs/03-build/c5-payout-dispatcher-build-prompts.md`), no code — written against a proposed `SigningService` interface and a fake, since S1 (key custody/signing) does not exist anywhere in this repo, not even as a design. See `docs/03-build/c1-scenario-catalog.md`, `depositwatcher/`, `screening/`, and `energybroker/`. |
| Next action | Two independent gaps left, not one linear next step. **A real AML vendor needs to be chosen and contracted** (Chainalysis/TRM/Elliptic, per `component-map.md`) before C3's pipeline and re-screen loops can be wired for real — discovery is already wired, since it needed no vendor. **S1 (key custody/signing) has to exist before C5 can be built for real** — nothing in this repo can sign a TRON transaction today, by design, and C5's own spec stands in a fake for it. On the business side, the week-2 wholesale pricing calls to Tronsell/Netts flagged in the findings doc are still not confirmed — C4 now polls real vendor prices live, which removes the code-correctness risk, but not the open question of whether retail-tier pricing still clears the modeled margin. |

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
  C1 today, what cross-component failure modes are still open (C3–C6, not yet built),
  and what's irreducible risk that has to be priced or insured rather than fixed.
  Audited against the real test suite on 1 Sep 2026.
- **[c2-deposit-watcher-build-prompts.md](docs/03-build/c2-deposit-watcher-build-prompts.md)** —
  the deposit watcher, specified the same way C1 was: sequenced build chunks
  (C2.0 → C2.10) with acceptance criteria, written for an AI coding agent. Opened
  with four prerequisite gaps against the already-built C1; the endpoint gap is
  now closed (C1.11) and the spec updated to match the shipped shape. **Built** —
  all chunks C2.0–C2.10 shipped, wired to a live chain-watching engine and
  verified against real BSC — see `depositwatcher/`.
- **[c3-screening-build-prompts.md](docs/03-build/c3-screening-build-prompts.md)** —
  screening, specified the same way as C1/C2: sequenced build chunks (C3.0 → C3.9)
  with acceptance criteria. **Built, discovery loop wired** — all chunks shipped,
  including C3.9's own replay ship-gate harness — see `screening/`. `cmd/screend`
  serves the manual-review/audit HTTP surface and now runs the discovery loop for
  real. The pipeline and re-screen loops stay unwired until a real AML vendor is
  chosen and contracted — vendor choice was deliberately left unmade, and wiring a
  mock into a production compliance path would be worse than the honest gap.
- **[c4-energy-broker-build-prompts.md](docs/03-build/c4-energy-broker-build-prompts.md)** —
  the energy broker, specified the same way as C1–C3: sequenced build chunks
  (C4.0 → C4.9) with acceptance criteria. **Built and production-wired** — all
  chunks shipped, including C4.9's own replay ship-gate harness, and `cmd/brokerd`
  is fully wired, including real Tronsell/Netts/CatFee HTTP clients and a real
  TronGrid on-chain reader. The doc's own "Read this fourth" addendum records why
  the original buffer design changed after those real vendor integrations were
  built — none of the three vendors' APIs support retargeting an existing
  delegation, which the first design assumed — see `energybroker/`.
- **[c5-payout-dispatcher-build-prompts.md](docs/03-build/c5-payout-dispatcher-build-prompts.md)** —
  the payout dispatcher, specified the same way as C1–C4: sequenced build chunks
  (C5.0 → C5.11). **Not built.** Written against the real, shipped C1–C4 code
  (not just their original specs, which had drifted) and against a *proposed*
  `SigningService` interface plus a fake, since S1 (key custody/signing) does not
  exist anywhere in this repo — not unfinished, never started. Also proposes a fix
  for a real atomicity gap this document found in C1's dispatching→held path.

### `ledger/`

The built C1 service — Go + PostgreSQL, no ORM, hand-written SQL, `pgx/v5`, `chi`
routing, `goose` migrations. `cmd/ledgerd` (the service), `cmd/migrate`, `cmd/replay`
(the C1.9 ship-gate harness), `cmd/seed-console` (seeds accounts for the manual test
console at `docs/console.html`). Packages under `internal/`: `money`, `accounts`,
`journal`, `orders`, `recon`, `halt`, `httpapi`, `replay`. Operational docs live at
`ledger/docs/`: `runbook.md`, `accounts.md`, `errors.md`, `openapi.yaml`.

### `depositwatcher/`

The built C2 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose` migrations.
`cmd/watcherd` (the live chain-watching engine), `cmd/migrate`, `cmd/replay`.
Packages under `internal/`: `addresses` (HD derivation), `chain` (ingestion,
finality, reorg detection), `candidates`, `orphaned`, `finality`, `ledgerclient`,
`httpapi`, `replay`, `money`. Operational docs at `depositwatcher/docs/openapi.yaml`.

### `screening/`

The built C3 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/screend` (the HTTP boundary plus the real discovery loop — see
above), `cmd/migrate`, `cmd/replay` (C3.9's own ship-gate harness).
Packages under `internal/`: `provider` (the vendor-agnostic AML interface),
`cache`, `verdict`, `discovery`, `pipeline`, `holds`, `rescreen`, `ledgerclient`,
`httpapi`, `replay`.

### `energybroker/`

The built, production-wired C4 service — Go + PostgreSQL, `pgx/v5`, `chi`
routing, `goose` migrations. `cmd/brokerd` (the full production server: pricing,
routing, the buffer, reservations, all wired), `cmd/migrate`, `cmd/replay`
(C4.9's own ship-gate harness). Packages under `internal/`: `provider` (the
vendor-agnostic interface plus real Tronsell/Netts/CatFee HTTP clients),
`pricing`, `routing`, `buffer` (including the real `TronGridReader` on-chain
verifier), `reservations`, `ledgerclient`, `httpapi`, `replay`.

## Reading order

If you are new to this: **findings → architecture decisions → component map → C1 build
prompts → scenario catalog → `ledger/` → C2 build prompts → `depositwatcher/` → C3
build prompts → `screening/` → C4 build prompts → `energybroker/` → C5 build
prompts.** Each document assumes the previous one is settled and does not re-open
it.

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
