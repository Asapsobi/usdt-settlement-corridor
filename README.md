# USDT Settlement Corridor

A specialised USDT cross-network settlement layer — BEP20 → TRC20 — sold not as a
swap service but as **TRC20 payout infrastructure for businesses**.

> *"Reliable TRC20 payouts, funded from any chain."*

This repository is the working record of the evaluation, the architecture decisions
that came out of it, and the build specifications derived from those decisions —
and, as of 8 Sep 2026, the ledger core, the deposit watcher, screening, the
energy broker, key management, and the payout dispatcher: every MVP service
except the API gateway (C6) is now built.

## Where things stand

| | |
|---|---|
| Market verdict | Not viable as "network conversion"; viable as payout infrastructure |
| Recommended model | **Model D** — conversion/payout API now; Model E (netting) year 2–3 |
| Margin engine | Wholesale TRON energy (25.7 sun blend vs 41 sun market) + batch multisend |
| Contribution margin | 87.1% at a $3,000 ticket |
| MVP scope | 6 services, one ledger, no smart contracts · ≈9–10 eng-weeks |
| Build status | **C1 (ledger core), C2 (deposit watcher), C3 (screening), C4 (energy broker), S1 (key management), and C5 (payout dispatcher) are built and tested.** C1: all chunks C1.0–C1.11 shipped, C1.9 replay gate passing at 10,000 orders / 32 workers, scenario catalog audited row-by-row against the real test suite (3 coverage gaps found and closed, one real HTTP-boundary bug found and fixed). C1.11 added the reversal/reorg HTTP surface (`POST /v1/entries/{id}/reversal`, `POST /v1/orders/{id}/reorg`) that C2 needs. C2: all chunks C2.0–C2.10 shipped, including the replay harness and HTTP boundary, wired to a live chain-watching engine and verified against real BSC. C3: all chunks C3.0–C3.9 shipped, including its own replay ship-gate harness; `cmd/screend` now also runs the discovery background loop for real (against a real, running C1 — no fake, no vendor dependency). The pipeline and re-screen loops are still not wired: both need a real `provider.ScreeningProvider` (an AML vendor client), and vendor choice among the Chainalysis/TRM/Elliptic candidates `component-map.md` names was deliberately left unmade — wiring a mock into a production compliance path would silently screen real orders against a fake verdict, which is worse than the honest gap. C4: all chunks C4.0–C4.9 shipped, including its own replay ship-gate harness (9/9 scenarios, 6/6 final assertions passing against a real ledger and real Postgres); `cmd/brokerd` is now fully wired to production, including real Tronsell/Netts/CatFee HTTP integrations and a real TronGrid on-chain reader, added after discovering none of the three vendors' real APIs support the delegation-retargeting the original buffer design assumed — see the "Read this fourth" addendum in `docs/03-build/c4-energy-broker-build-prompts.md`. Going live still needs the real payout slot addresses, a confirmed Tronsell base URL, and a whitelisted Netts egress IP — all operator-supplied, none fabricated. C5: all chunks C5.0–C5.11 shipped, including its own replay ship-gate harness (11/11 scenarios, 6/6 final assertions passing against a real ledger and real Postgres) — see `dispatcher/`. Signs against S1's real, shipped request/poll `SigningService` contract, but S1 itself is still an in-process fake in every test and in the ship gate (no real cloud KMS exists behind S1 yet — see S1's own entry below), so nothing in this component has touched mainnet. Sweep-tier batching (`internal/txbuild.BuildMultisend`, `CutBatch`/`BroadcastBatch`/`HandlePartialSettlement`) is built against a *proposed* multisend contract interface (modeled on disperse.app's own real, audited contract shape) plus a fake standing in for it — no smart contract exists on either chain per decision 1, and none is deployed here. Building C5 required one small, contained addition to already-shipped C1: `POST /v1/accounts` (idempotent on code), since nothing let a remote caller create the per-customer/per-slot ledger accounts C5 references before referencing them. Testing against a real ledgerd (not just fakes) caught two real, previously-invisible bugs: `PostReversal` never sending the `Idempotency-Key` header C1's own middleware requires on every write route, and `EnterDispatching` computing a fresh `occurred_at` on every retry instead of a caller-supplied stable one, which broke C1's own idempotent-replay check on a legitimate crash-recovery retry — both fixed. Going live needs everything S1 needs (below) plus the real multisend contract designed, audited, and deployed, and the 35,000-per-recipient energy estimate independently measured against a real testnet payout, not assumed. S1: all chunks S1.0–S1.6 shipped, including its own replay ship-gate harness (5/5 scenarios, 4/4 final assertions passing against real Postgres) — see `s1/`. Signs against a real, independently-verified secp256k1/TRON signing path (DER↔compact conversion, recovery-id resolution against KMS's own missing `v` value) and a real 2-of-N human-approval queue, both tested against an in-process `FakeKMSClient` standing in for real cloud KMS — there is no real AWS/GCP adapter yet, and `cmd/s1d` refuses to start without `S1_KMS_CLIENT` set (no default, so a misconfigured deployment fails loud rather than signing real payouts against a fake key). The replay ship gate itself caught one real spec bug during the build (a "names exactly 2 approvers" assertion that a legitimate KMS-failure-retry scenario violates) — fixed in both the code and the build doc. Going live needs the real KMS key-generation ceremony (`docs/02-architecture/s1-key-custody-architecture.md`'s own "Key generation and bootstrapping" — an operational procedure with real cloud credentials, not something built here) and a real KMS adapter behind `kmssign.KMSClient`. See `docs/03-build/c1-scenario-catalog.md`, `depositwatcher/`, `screening/`, and `energybroker/`. |
| Next action | Four independent gaps left, not one linear next step. **S1 needs a real cloud KMS adapter and the actual key-generation ceremony** before C5 can sign anything for real — the code and both services' own ship gates are done, the cloud-account work isn't (and can't be done from here). **The proposed multisend contract C5's own Sweep tier depends on needs to be designed, audited, and deployed** (or the tier's own margin claim — decision 6's "batching halves per-recipient energy" — needs to be re-examined without it), and its real per-recipient energy cost measured against testnet rather than assumed at 35,000. **A real AML vendor needs to be chosen and contracted** (Chainalysis/TRM/Elliptic, per `component-map.md`) before C3's pipeline and re-screen loops can be wired for real — discovery is already wired, since it needed no vendor. On the business side, the week-2 wholesale pricing calls to Tronsell/Netts flagged in the findings doc are still not confirmed — C4 now polls real vendor prices live, which removes the code-correctness risk, but not the open question of whether retail-tier pricing still clears the modeled margin. C6 (API gateway) is the one MVP service with no code yet at all. |

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
- **[s1-key-custody-architecture.md](docs/02-architecture/s1-key-custody-architecture.md)** —
  the custody decision record component-map only ever named, never designed:
  self-hosted cloud KMS over a third-party custodian, six independent TRON
  slot keys (never a shared HD seed, for the same isolation reason decision 4
  rejected a pooled treasury), a hybrid threshold splitting signing requests
  into auto-sign versus 2-of-N human approval, the actual TRON-over-KMS
  signing mechanics (DER→compact, recovery-id resolution), and an explicit
  threat model naming what this design does and doesn't defend against.

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
  (C5.0 → C5.11). **Built** — all chunks shipped, including C5.11's own replay
  ship-gate harness (11/11 scenarios, 6/6 final assertions passing against a
  real ledger and real Postgres) — see `dispatcher/`. Originally written against
  a *proposed* synchronous `SigningService` interface plus a fake, since S1 did
  not exist yet even as a design; that proposal was **superseded** by
  s1-key-management-build-prompts.md's async, request/poll version before any
  C5 code was written, so the shipped component signs against S1's real,
  shipped contract (S1 itself is still a fake everywhere, pending a real KMS
  adapter). Also proposed a fix for a real atomicity gap this document found in
  C1's dispatching→held path (`POST /v1/orders/{id}/dispatch-failure`) — not yet
  built into C1; C5 ships against the interim two-call sequence the doc itself
  names as the fallback. C5.8's own Sweep-batching chunk surfaced a second real
  contradiction — decision 1 ("no smart contracts on either chain") versus
  decision 6's batching-margin claim, which needs one to exist — resolved the
  same way: build against a proposed contract interface and a fake, real
  deployment left as an explicit open item.
- **[s1-key-management-build-prompts.md](docs/03-build/s1-key-management-build-prompts.md)** —
  key management, specified the same way as C1–C5: sequenced build chunks
  (S1.0 → S1.6) turning `s1-key-custody-architecture.md`'s decisions into code.
  **Built** — all chunks shipped, including S1.6's own replay ship-gate harness,
  which caught one real spec bug (an over-strict "exactly 2 approvers" final
  assertion a legitimate retry scenario violates) during the build itself — see
  `s1/`. Replaces C5's original synchronous `Sign` proposal with a request/poll
  `SigningService` (mirroring C4's own PENDING→CONFIRMED reservation shape)
  since a human-approval path can legitimately take minutes to hours, which a
  synchronous call can't wait on safely. No real cloud KMS adapter yet — signs
  against `kmssign.KMSClient`, real for everything except the actual AWS/GCP
  call.

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

### `s1/`

The built S1 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/s1d` (the HTTP boundary — two separate bearer-auth scopes,
one for C5's own signing calls, one for human approvers; refuses to start
without `S1_KMS_CLIENT` set, no default), `cmd/migrate`, `cmd/replay` (S1.6's
own ship-gate harness). Packages under `internal/`: `kmssign` (the *only*
package that may hold or produce signing-capable code — `KMSClient` interface,
`FakeKMSClient`, and `Wrapper`, the real TRON-over-KMS signing mechanics —
enforced by its own dependency test), `slots` (the key registry, deriving each
slot's real TRON address from its KMS public key), `requests` (the
`SigningService` queue: auto-sign under threshold, 2-of-N human approval at or
above it), `httpapi`.

### `dispatcher/`

The built C5 service — Go + PostgreSQL, `pgx/v5`, `chi` routing, `goose`
migrations. `cmd/dispatchd` (the HTTP boundary — `POST /v1/dispatch` performs
only the synchronous half of a dispatch, slot selection plus the E2 conversion
entry; carrying an order the rest of the way is a background worker this
command does not yet start), `cmd/migrate`, `cmd/replay` (C5.11's own
ship-gate harness). Packages under `internal/`: `slots` (the slot-identity
registry, cap-checked selection, and real Tether-blacklist freeze detection —
verified live against USDT-TRC20's own `isBlackListed(address)`), `txbuild`
(offline TRC20 transfer and proposed-multisend construction — both verified,
where a real contract exists, against a live TRON node), `signing` (the S1
client), `energy` (the C4 client), `dispatch` (the state machine: entering
dispatching, broadcast with proven exactly-once semantics, finality
confirmation, non-retryable-failure handling and reconciliation, Sweep
batching, freeze handling), `ledgerclient`, `httpapi`, `replay`.

## Reading order

If you are new to this: **findings → architecture decisions → component map → C1 build
prompts → scenario catalog → `ledger/` → C2 build prompts → `depositwatcher/` → C3
build prompts → `screening/` → C4 build prompts → `energybroker/` → S1 architecture
→ S1 build prompts → `s1/` → C5 build prompts → `dispatcher/`.** Each document
assumes the previous one is settled and does not re-open it.

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
