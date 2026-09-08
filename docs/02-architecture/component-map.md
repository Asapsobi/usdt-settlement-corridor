# Component map — build decomposition and dimensions

_v1.0, 31 Aug 2026. Decomposition of decision 1 in `docs/02-architecture/product-operations-architecture.md` ("six services, one ledger, no smart contracts"). This is the build-order and scoping document; it does not re-open any architecture decision._

## The six services + three supporting pieces

| # | Component | One-line purpose | Eng-weeks | Risk | Phase |
|---|---|---|---|---|---|
| C1 | **Ledger core** | Single source of truth for money and order state | 1.5 | High (invariants) | MVP — first |
| C2 | **Deposit watcher (BSC)** | Turn inbound BEP20 USDT into confirmed, attributed credits | 1.5–2 | High (reorg correctness) | MVP |
| C3 | **Screening** | Release / hold decision on deposit provenance | 1.0 | Low–med (vendor) | MVP |
| C4 | **Energy broker** | Acquire TRON energy at the 25.7 sun blend before every payout | 1.5 | Med (3 vendors, timing race) | MVP |
| C5 | **Payout dispatcher (TRON)** | Deliver USDT to recipient and confirm to SR finality | 2.0 | High (money out) | MVP |
| C6 | **API gateway** | Customer contract: quote → order → status → webhook | 1.5 + 0.5 sandbox | Med | MVP |
| S1 | Key management / signing | BSC HD seed, 6 TRON slot keys, split custody | 0.5–1.0 | High (terminal) | MVP, cross-cutting |
| S2 | Treasury rebalancer | 4-hour loop, 65/35 target | 0 (runbook + dashboard) | Med | Service in Phase 2 |
| S3 | Status page | P50/P95/P99 by tier — the primary sales asset | 1.0 | Low | After first real traffic |

MVP total ≈ 9–10 eng-weeks ≈ two engineers × 5 weeks, inside the 6–8 week envelope in the findings doc.

## Dependency graph and build order

```
C1 Ledger ──┬─→ C2 Deposit watcher ──→ C3 Screening ──┐
            │                                          ├─→ C5 Payout dispatcher ──→ S3 Status page
            ├─→ C4 Energy broker ─────────────────────┘
            └─→ C6 API gateway
                 S1 Keys — required before C5 touches mainnet
```

C6 and C4 can be built in parallel with C2 once C1's journal-entry contract is frozen. Nothing may be built before C1, because C1's entry shape is every other component's interface.

## Per-component dimensions

### C1 — Ledger core
- **Owns:** account tree (customer liability · per-slot TRON asset · per-deposit-address BSC asset · custody suspense · energy expense · fee revenue), append-only balanced journal, order state machine, reconciler, halt signal.
- **Does not own:** chain access, keys, pricing, customer auth.
- **Interface:** idempotent `POST /entries`, `GET /balances`, `POST /orders/{id}/transition`, halt broadcast.
- **State machine:** `quoted → funded → screened → dispatching → settled | refunded | held`.
- **Gate:** 10k-order replay including duplicate idempotency keys, reorg reversals, partial batch failures — balances tie, halt fires on injected drift.

### C2 — Deposit watcher (BSC)
- **Owns:** HD address derivation + assignment/retirement, block cursor, rolling block-hash window for reorg detection, Transfer-log parsing for the BEP20 USDT contract only, amount classification (exact / under / over / dust), 15-confirmation finality declaration.
- **Does not own:** sweeping deposits, screening, payout, pricing, address funding with BNB.
- **Emits:** `deposit.detected` (0-conf, advisory only) · `deposit.final` (15-conf, credits the ledger) · `deposit.reorged` (reversing entry).
- **Scale dimension:** at 100 payouts/day ≈ 100 deposits/day, ~4/hour peak; one address per order, ~36k addresses/year derived. Trivial throughput — this component is about *correctness*, not scale.
- **Hard parts:** reorg deeper than the confirmation window; RPC provider divergence (needs ≥2 providers with hash agreement before finality); duplicate log delivery; deposits to retired addresses; wrong-token and zero-value transfers; deposits arriving after quote expiry.
- **Verify before coding:** current BSC block time and therefore the wall-clock cost of 15 confirmations — this is a direct input to the tier SLAs.

### C3 — Screening
- **Owns:** provider call (Chainalysis / TRM / Elliptic), result caching by sender address, hold queue, manual review path, reason codes.
- **Does not own:** the release action itself — it returns a verdict; the ledger transitions the order.
- **Dimension:** ~100 calls/day at MVP volume. Cost per call is a real line item at scale; cache by sender.

### C4 — Energy broker
- **Owns:** routing 60/35/5 across Tronsell / Netts / CatFee, price polling with a ceiling, pre-order lead time, delegation confirmation, fallback ladder, per-payout cost attribution to the ledger.
- **Does not own:** broadcasting the USDT transfer.
- **Hard parts:** delegation must land *before* broadcast or the payout burns TRX at market rate; provider outage mid-flight; price spike above the ceiling (must be able to hold rather than overpay).
- **Blocked on:** the week-2 wholesale pricing calls. Retail-quote pricing invalidates the routing weights.

### C5 — Payout dispatcher (TRON)
- **Owns:** slot selection under the $50k / 5,000-tx caps, batch window for the Sweep tier, multisend construction, signing, broadcast, SR-finality confirmation, retry with exactly-once semantics.
- **Does not own:** energy acquisition, screening.
- **Hard parts:** energy exhausted at broadcast; slot frozen mid-flight; duplicate broadcast on retry; partial batch success in multisend; slot rotation without stranding balance.
- **Testnet dependency:** the week-6 batched-multisend energy measurement (35,000/recipient assumption) is measured here — this component's own energy-per-recipient config, not yet independently confirmed.
- **Built** — `docs/03-build/c5-payout-dispatcher-build-prompts.md` (C5.0–C5.11), see `dispatcher/`. Signs against S1's real request/poll `SigningService` contract but an in-process fake standing in for S1 itself (no real cloud KMS behind S1 yet, so nothing in this component can touch mainnet). Sweep-tier batching is built against a *proposed* multisend contract interface (no smart contract exists on either chain per decision 1 — see the doc's own C5.8 discussion) and a fake standing in for it. Required a small addition to C1 (`POST /v1/accounts`, idempotent-on-code) so this component can ensure the per-customer/per-slot ledger accounts it references actually exist — C1 had no way to create an account over HTTP before.

### C6 — API gateway
- **Owns:** auth, rate limits, quote issuance with the 90s lock, idempotency keys, HMAC-SHA256 webhook signing with 8 retries, sandbox with the four deterministic failure triggers (reorg, screening hold, energy exhaustion, retry storm).
- **Does not own:** money movement; calls a pricing library rather than embedding tiers.
- **Rule:** quote-then-order, never quote-inside-order.

### S1 — Key management / signing
- **Owns:** custody of the six TRON slot keys and the BSC HD seed, the only signing capability anywhere in this system.
- **Does not own:** transaction construction, slot selection/caps/rotation (C5's own slot-identity registry), broadcast.
- **Design:** `docs/02-architecture/s1-key-custody-architecture.md` — self-hosted cloud KMS, six independent slot keys, a hybrid auto/2-of-N-human-approval threshold. **Built** — `docs/03-build/s1-key-management-build-prompts.md` (S1.0–S1.6), see `s1/`. No real cloud KMS adapter yet; signs against an in-process fake standing in for one.

## What is deliberately not built at MVP

- Treasury rebalancing as a service — a runbook plus a balance dashboard is correct until the 4-hour loop is proven sustainable (month 3 test).
- Any smart contract on either chain.
- A public swap page.
- Reverse flow (TRC20 → BEP20) — it arrives with the capital ladder, not before.
