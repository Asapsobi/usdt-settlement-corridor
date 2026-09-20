# Admin/Operations Panel — build prompts (OC.10 → OC.15)

**Target:** Go, extending the already-shipped Ops Console. **Consumer:** an AI
coding agent (Claude Code or equivalent). **This is not a new service.** It is
chunks **OC.10 onward**, continuing `ops-console-build-prompts.md`'s own
numbering (OC.0–OC.9 are shipped — see `opsconsole/`) — paste that document's
§0 first if the agent's context doesn't already have it, then paste this
document's own preamble below, then work chunk by chunk.

---

## Read this first — what already exists, so you don't rebuild it

`opsconsole/` is a real, shipped Go service (chi router, `html/template`
rendering, no database of its own) with:

- Session auth via a signed cookie, entered once at login (`internal/session`) — OC.1.
- Six typed HTTP clients to C1–C5/S1 (`internal/opclient/{ledger,watcher,screening,broker,dispatcher,s1}.go`).
- A home page with a service-health grid and halt banner, built on every service's
  own `GET /v1/system/invariants` — OC.2.
- Ledger halt control, C2 cursor view, C4 reservations-by-status + reconcile,
  C3 holds queue, C5 slot list, S1 pending-approvals queue — OC.3–OC.7.
- An append-only audit log, written server-side before every downstream write —
  `internal/auditlog` — OC.8.
- A real integration ship gate against live subprocesses — OC.9.

Every chunk below **adds to this**, using the same `internal/opclient` clients,
the same session/audit/error conventions, the same server-rendered
`html/template` + vanilla-JS style — no new frontend stack, no new database,
no direct Postgres connection to any service (all five non-negotiable
invariants in `ops-console-build-prompts.md` §0 still apply, unchanged,
including invariant 5: **no secret is ever rendered into an HTML page, a log
line, or a URL** — this is load-bearing for OC.11 below, not a formality).

---

## Read this second — item 6 as written cannot be built, and should not be

The original ask includes: *"the seed of that wallet, and the ability to
generate the private key simply by pressing a button... so that the operator
has access to all of these wallets and can manually sweep that wallet
themselves."*

**There is no private key to retrieve, for either wallet type this system
has, and that's a deliberate property, not a gap:**

- **TRON treasury/payout slots (C5's six slots):** signed by AWS/GCP KMS per
  `s1-key-custody-architecture.md`. Each slot's key is generated **inside**
  KMS with **no export capability, ever** — the document's own "Why this, not
  a custodian" section spends a paragraph on exactly this property: *"the
  private key never leaves a hardware security boundary, ever, not even to
  the process that requests a signature."* S1's `SigningService` interface
  (`RequestSignature`/`GetSignature`) takes an unsigned transaction and
  returns a signature — nothing in its contract, or in KMS's own API surface,
  can return key material. Building a "reveal private key" button would mean
  either (a) it's a dead button that can never work against the real system,
  or (b) someone re-architects S1 to extract and store raw keys outside KMS,
  which is precisely the threat model's own "Defends against" list item #1 —
  undone.
- **BSC deposit addresses (C2):** derived from an extended **public** key
  (`WATCHER_XPUB`) only. `depositwatcher/internal/addresses` is, by its own
  design and its own `no_signing_test.go`, structurally incapable of parsing
  or holding a private key. The BSC HD seed itself lives offline, in cold
  storage, and is explicitly out of every running service's reach at MVP —
  sweeping BSC balances is deferred to a later phase (S2) and today is a
  manual runbook, not an automated capability anywhere in this codebase.

**The real need behind item 6 — "sweep a wallet without hunting for its
key" — is already solved, correctly, by S1's existing signing flow, for the
one wallet type that can be swept today (TRON slots):** an operator supplies
a destination address, the console builds an unsigned sweep transaction,
calls `S1Client.RequestSignature`, and — same as every other payout — it
either signs immediately (under the approval threshold) or drops into the
existing 2-of-N approval queue this console already has a page for (OC.7).
No key is ever displayed, copied, or held anywhere outside KMS. **This is
strictly more convenient than a private-key button** — the operator never
touches key material at all, for any wallet, ever — and it's the version
built below (OC.11). If the account owner wants literal raw-key custody
outside KMS for some other reason, that's a re-decision of
`s1-key-custody-architecture.md` itself, made deliberately and in that
document, not a UI feature bolted on here.

For BSC deposit addresses, OC.11 shows balances and flags anything worth
sweeping, and links out to the existing manual runbook — it does not
attempt to automate a signing capability that does not exist in this system.

---

## Read this third — some of this needs a small, real backend addition first

Same posture `ops-console-build-prompts.md`'s own OC.4/OC.5/OC.7 already
took: a small, additive, backward-compatible route on an already-shipped
service, never a redesign. Verify each of these against the owning service's
current `docs/openapi.yaml` before writing the UI chunk that depends on it —
they are correct as of the routes below, checked directly against each
service's real `openapi.yaml` on `main`, but re-check, the same "trust the
code over any doc" discipline this whole repo already applies to itself:

1. **C2 has no manual deposit-confirmation route and no exposed confirmation
   counter.** `depositwatcher`'s real routes are `GET/POST /v1/addresses`,
   `GET /v1/addresses/{order_id}`, `POST /v1/addresses/{order_id}/retire`,
   `/v1/orphaned-deposits`, `/v1/system/{cursor,invariants,providers}` —
   nothing takes an operator-supplied txid. OC.12 adds
   `POST /v1/addresses/{order_id}/confirm-deposit` (body: `tx_hash`) to
   depositwatcher: it looks the transaction up against BSC directly (the
   same RPC path C2's own ingestion loop already uses), verifies it actually
   pays the order's own deposit address for at least the expected amount,
   and — only if real — feeds it through the exact same code path a normal
   detected deposit uses (so it still respects finality/confirmation rules
   downstream; this is a manual **trigger to look**, not a manual override
   that skips verification). If C2's ingestion internals make "look up one
   specific tx and verify it" meaningfully different from "wait for the
   scanner to see it," say so and propose the smallest honest alternative
   instead of faking the check.
2. **C4 has no concept of "balance held at a vendor" and no route to rent
   energy for, or delegate energy to, a specific wallet on demand.** Real C4
   routes are `/v1/reservations`, `/v1/buffer`, `/v1/manual-fallback-events`,
   `/v1/system/{prices,invariants}` — reservations are *per-order*, and the
   buffer is a pooled inventory, not a per-vendor account balance. Before
   building OC.14's balance panel, confirm with whoever owns the vendor
   relationships whether Tronsell/Netts/CatFee even expose an
   account-balance API call today — if not, this line item is a vendor-side
   gap, not a C4 gap, and the panel should say so rather than show a fake
   zero. For manual rent/delegate: `c4-energy-broker-build-prompts.md`'s own
   "Read this fourth" addendum found that **none of the three real vendor
   APIs support retargeting an existing delegation** — confirm this is still
   true before promising "delegate energy from wallet A to wallet B" as a
   real feature; if it's still true, OC.14's delegate action can only mean
   "buy a fresh delegation to wallet B," not "move B's existing energy from
   A," and the UI must say which one it's doing.
3. **No service links an order to its own reservation/signing-request by
   id.** `product-operations-architecture.md`'s ground-truth check already
   found this: C1's `Order` has no `reservation_id`/`signing_request_id`
   field. OC.10's aggregated order view is built by the **console** joining
   on `external_id`/`order_id` across C1+C2+C4+C5+S1 responses — it is not,
   and should not become, a new field added to C1's own `Order`.
4. **C5's slot list is the only real "treasury wallet" registry that
   exists.** There is no separate concept of "energy-renter wallet" as a
   distinct on-chain address in what's built — C4 pays vendors from
   whatever account funds its API relationship, not from a slot. If a
   dedicated on-chain "energy funding wallet" exists operationally, it isn't
   modeled in any service today; OC.11's wallet registry should say this
   plainly rather than inventing a role no service actually tracks.

---

## Read this fourth — the completeness bar, and where it's not met yet

The account owner's own bar for this panel: **there should be nowhere in the
product this admin panel cannot see, and nowhere it cannot act, if a
capability to act genuinely exists.** Take that literally — not "cover the
six workflows described," which is a subset. Two consequences:

1. **Every real route on every one of the seven services (C1–C6, S1) needs a
   home in this panel** — a view if it's a read, a form if it's a write —
   not just the routes the six original items happened to describe. The
   coverage matrix at the end of this document is the acceptance artifact
   for that claim: it is not done if a row in that table has no chunk
   number next to it.
2. **"Full access" means full access to what the system can actually do,
   not a promise to add capability the system doesn't have.** "Read this
   second" already drew this line for private keys — it's the same line
   here, applied generally: if a service genuinely has no route to do
   something (gateway has no admin surface at all today — see OC.19), the
   honest fix is to add that route to the owning service, same as OC.4/
   OC.5/OC.7/OC.12 already do, not to fake a UI over a capability that
   doesn't exist. Where doing so is a real, deliberate re-architecture
   (KMS export) rather than a small additive route, this document says so
   explicitly and does not build it — a panel that lies about its own
   access is worse than one that names its limits.

Checked against the real routes just now (each service's own
`docs/openapi.yaml`, or `internal/httpapi` for S1), two gaps are large
enough to call out here rather than let the coverage matrix be the first
place they surface:

- **C1's own journal is invisible today.** Halt control exists (OC.3); raw
  entry browsing (`GET /v1/entries`), manual reversal
  (`POST /v1/entries/{id}/reversal`), manual reorg handling
  (`POST /v1/orders/{external_id}/reorg`), trial balance, and reconciliation
  snapshots have no page at all. OC.16 closes this.
- **C6 (the customer-facing gateway) has no operator surface whatsoever.**
  `opsconsole/internal/opclient` has six clients — C1 through C5 and S1 —
  and deliberately not a seventh: gateway was never brought into this
  console's scope. Its real routes (`/v1/orders`, `/v1/quotes`,
  `/v1/sandbox/orders`) are customer-scoped by API key, not operator
  routes, and nothing lets an operator issue or revoke an `sk_live_`/
  `sk_test_` key, inspect webhook delivery/retry state, or see rate-limit
  counters — because gateway's own code has no such route today, for
  anyone. OC.19 adds a `c6client` and the gateway-side admin routes it
  needs — the single largest real gap this review found, larger than any
  of the six originally-listed items.

---

## OC.10 — Order detail view: real state, real blockers, real alerts

```
Extends OC.2's home grid with a per-order drill-down and a corridor-wide
alert banner, using only fields that are real today.

BUILD internal/httpapi/orderdetail.go
  GET /orders  (list/search — no such view exists anywhere today; every
                existing OC chunk assumes you already know an id)
    - Thin wrap of C1's own GET /v1/orders, exposing whatever filter/
      pagination params that route already takes (confirm against C1's
      openapi.yaml — don't invent query params it doesn't support).
      This is the entry point an operator without a specific id in hand
      actually needs, and nothing before OC.10 provides it.

  GET /orders/{external_id}
    - Calls LedgerClient.GetOrder (the canonical state + amounts + tier).
    - Calls WatcherClient.GetAddress(order_id) for deposit-address status
      (WATCHING/FUNDED/RETIRED) -- this is the sub-stage detail *within*
      `quoted`, per this document's own "Read this third" item 3, joined
      by the console, not fetched as one object.
    - If state >= dispatching: calls DispatcherClient.GetDispatch(order_id)
      for latest_attempt_status and dispatch_attempts history.
    - Best-effort: if a reservation or signing-request id is discoverable
      (only C5's own dispatch record may reference these -- confirm the
      real field name against dispatcher's current code before assuming
      it exists), fetch and show those sub-statuses too. If no such link
      exists, show "no linked reservation/signing-request found" rather
      than silently omitting the section -- an operator debugging a stuck
      order needs to know the console looked and found nothing, not
      wonder if it forgot to check.
    - "Blocker" is DERIVED, never invented: an order in `held` shows C3's
      real hold reason (GET /v1/holds?order_id=); `dispatching` with a
      FAILED reservation shows C4's own failure reason; `dispatching` with
      a PENDING signing request over threshold shows "awaiting N of 2
      approvals" from S1's real signing_approvals count if exposed, or
      "awaiting approval" if not. Never fabricate a blocker reason a
      downstream service doesn't itself report.

  GET /alerts  (corridor-wide banner, extends OC.2's per-service health grid)
    - Aggregates each service's own GET /v1/system/invariants -- reuses
      OC.2's existing calls, does not re-implement "what counts as
      unhealthy" (that's each service's own definition, per this doc's own
      "no new business logic" rule).
    - Adds one cross-service check this console IS positioned to make
      (because it already calls all six services, and no single service
      can see this on its own): an order stuck in the same state for
      longer than a configurable threshold (OC_STUCK_ORDER_MINUTES) with
      no matching in-flight reservation/signing-request/dispatch-attempt
      anywhere -- i.e. genuinely orphaned, not just slow. This is a real
      new check, name it as such in the UI ("console-detected: possibly
      orphaned"), never blend it into a service's own invariant list as if
      that service reported it.

ACCEPTANCE
- Order detail renders correctly for an order in each of the 6 real C1
  states, including `refunded` (must show C3's own 501
  refund_entry_not_implemented as a real, visible limitation -- never hide
  it or show a fake "refunded" success).
- One downstream service down: that section of the page shows a degraded
  panel (invariant 4), the rest of the page still renders.
- The orphaned-order check has a table-driven test against fakes proving
  it fires only when genuinely no downstream record exists, not merely
  when one call times out (a timeout is "unknown," not "orphaned" --
  don't conflate them).
```

---

## OC.11 — Wallet registry and sweep-via-S1 (replaces the private-key ask)

```
The corrected version of item 6, plus item 1's wallet list — see "Read this
second" above before writing a line of this chunk.

BUILD internal/httpapi/wallets.go
  GET /wallets
    - TRON treasury/payout slots: DispatcherClient.ListSlots() -- id,
      tron_address, status, retired-or-not. Balance: call whatever real
      on-chain balance read this project already has (C4's TronGrid reader
      in energybroker, or a slot-balance route if C5 exposes one -- verify
      which; do not add a second TRON RPC client to opsconsole if one
      already exists to call into).
    - BSC deposit addresses: WatcherClient.ListAddresses(), grouped by
      status. These are receive-only by construction (see "Read this
      second") -- label them as such in the UI, with no sweep action.
    - Role column is exactly {tron_slot, bsc_deposit_address} -- do not
      invent a third role (e.g. "energy renter wallet") that no service
      actually tracks, per "Read this third" item 4. If C4 or ops later
      exposes a real funding-wallet concept, extend this table then.
    - Seed/private-key column: literally absent from this page. Not
      redacted, not masked with asterisks -- not a field that exists here,
      because invariant 5 (ops-console §0) forbids ever rendering a secret,
      and for TRON slots the key materially cannot be retrieved at all
      (see "Read this second").

  POST /wallets/{tron_address}/sweep
    - Form: destination_address (required), confirm (checkbox, required).
    - Resolves tron_address to its slot_id (DispatcherClient), builds an
      unsigned sweep transaction for that slot's full spendable balance
      minus a configured minimum reserve for the slot's own future energy
      needs (do not sweep a slot to zero and strand its own next payout --
      confirm the real reserve-amount convention against C5's own slot
      logic rather than inventing a number).
    - Calls S1Client.RequestSignature with that unsigned tx. Renders
      PENDING (with a link to OC.7's existing approval queue) or SIGNED
      (with the resulting txid) -- exactly the same two outcomes every
      other signing request in this system already has. Broadcasting a
      SIGNED sweep tx reuses whatever broadcast path C5 already has
      (confirm whether dispatcher exposes a generic broadcast route, or
      whether this needs its own thin TRON broadcast call mirroring C5's
      -- do not duplicate C5's broadcast logic if a route already exists).
    - Audit-logged before the S1 call is made (invariant 3), same as every
      other write in this console.

ACCEPTANCE
- The wallet list renders both wallet types with a role column, no key
  material column, and a real balance where one is available.
- Sweep on a slot under the approval threshold: completes and shows a real
  txid. Sweep at or over threshold: shows PENDING and appears in OC.7's
  queue, requiring 2 distinct approvers exactly as any other signing
  request does -- write a test proving a sweep cannot be pushed through
  with a single approval, the same guarantee every other payout has.
- Attempting to sweep a BSC deposit address is not an option in the UI at
  all (no route, no button) -- not a disabled button with a tooltip, an
  absent feature, because the capability genuinely does not exist.
- grep-based test: no template in this chunk contains the strings
  "private key," "seed," or "mnemonic" anywhere in rendered output.
```

---

## OC.12 — Deposit watch board + manual txid confirmation

```
Item 2. Requires the new C2 route from "Read this third" item 1 -- build
that first, as its own small, tested addition to depositwatcher, before
the console page that calls it.

BACKEND (depositwatcher, not opsconsole)
  POST /v1/addresses/{order_id}/confirm-deposit   { "tx_hash": "0x..." }
    - Fetches the tx from BSC (same RPC client C2's ingestion loop uses).
    - Verifies: tx is confirmed on-chain, pays the order's own deposit
      address (from this order's own addresses row), for a USDT_BEP20
      Transfer of at least the order's expected amount_in.
    - On success: feeds it through the SAME crediting path a normally-
      detected deposit uses (do not write a second, parallel "manually
      credited" code path -- one path, two entry points, matching this
      project's own idempotency discipline elsewhere).
    - On failure (wrong address, wrong amount, not yet confirmed, tx not
      found): a named error, never a silent no-op.
    - Idempotent: submitting the same tx_hash twice does not double-credit
      -- this is exactly what the normal path's own idempotency key
      already guards, reuse it, don't invent a second guard.

BUILD internal/httpapi/deposits.go (opsconsole)
  GET /deposits/watching
    - Lists addresses with status WATCHING or FUNDED-but-not-yet-final,
      from WatcherClient.ListAddresses(status=...).
  POST /deposits/{order_id}/confirm  { tx_hash }
    - Calls the new C2 route above. Audit-logged first (invariant 3).
      Shows the real success/failure the route returns -- never a
      generic "submitted."

ACCEPTANCE
- C2: a table-driven test proving confirm-deposit accepts a real matching
  tx, rejects a tx to the wrong address, rejects an underpaid tx, and is
  idempotent on repeated submission of the same tx_hash.
- Console: submitting a bad txid shows the real rejection reason from C2,
  not a generic error. Submitting a good one shows the order's own state
  actually advancing (funded), verified against a real running watcherd
  in the integration suite (OC.9's existing pattern, extended).
```

---

## OC.13 — AML / counterparty exposure view

```
Item 3. Built entirely by cross-referencing existing C3 + C2 + C1 data --
no new backend route, per invariant "no new business logic to invent": C3
already decides what's flagged, this chunk only visualizes it.

BUILD internal/httpapi/aml.go
  GET /aml/exposure
    - Pulls C3's own flagged entities: GET /v1/holds (active AML holds) and
      GET /v1/screening-results (filtered to non-clean verdicts) --
      whatever field names those real responses use for the counterparty
      address, use exactly those, don't rename them.
    - For each flagged counterparty address, cross-references
      WatcherClient.ListAddresses() / GetAddress() to find which of OUR
      deposit addresses that counterparty actually paid -- this is the
      "both sides shown together" requirement: (their flagged address) ->
      (our deposit address) -> (the order_id it funded) -> (that order's
      real C1 state, e.g. held).
    - Groups by counterparty so one flagged sender touching multiple orders
      shows as one row with multiple linked orders, not N duplicate rows.
    - This is a READ-ONLY view. Releasing or rejecting a hold is already
      OC.6's job (existing) -- do not duplicate that action here, link to
      it instead.

ACCEPTANCE
- Given a fake C3 response with 2 flagged addresses and a fake C2 response
  showing one of them funded 3 different orders, the view groups correctly
  into 2 counterparty rows, one with a 3-order list.
- A flagged counterparty with no matching C2 record (flagged before ever
  reaching this system, or flagged by a mechanism other than a deposit --
  confirm which is possible per C3's own design) renders as "flagged, no
  linked deposit found" rather than being silently dropped from the list.
```

---

## OC.14 — Energy broker operations

```
Item 4. Read "Read this third" item 2 before writing this chunk -- the
per-vendor balance and wallet-to-wallet delegate features may not be
buildable as literally asked, against real vendor capability.

BUILD internal/httpapi/broker_ops.go
  GET /broker/overview  (extends OC.5's existing reservations-by-status page)
    - Adds BrokerClient's GET /v1/buffer (already a real route) rendered
      as current buffer level vs. target, by provider.
    - Vendor account balance: only build this row if you've confirmed (per
      "Read this third" item 2) that C4 or a vendor API actually exposes
      one. If not, the row reads "not available -- vendor APIs surveyed
      for this build do not expose an account-balance call" rather than a
      fabricated number.

  POST /broker/wallets/{tron_address}/rent
    - Form: energy_units, max_price_sun (respects C4's own ceiling
      invariant -- never submit a request that could pay above whatever
      ceiling C4 itself enforces; if C4 has no route for "buy for a
      specific address outside the normal reservation flow," this is a
      new C4 route to propose, not something opsconsole fakes by misusing
      the existing POST /v1/reservations shape meant for order-driven
      demand).

  POST /broker/wallets/{from}/delegate
    - Per "Read this third" item 2: if real vendor APIs can't retarget an
      existing delegation, this action can only mean "buy a fresh
      delegation targeted at `to`," and the button/label must say that --
      never call it "delegate from A" if A's own energy isn't what moves.

ACCEPTANCE
- Buffer-by-provider view matches a fake GET /v1/buffer response exactly.
- The rent action, given a fake C4 ceiling, refuses (with a clear message)
  a request priced above it -- never silently clamps and submits anyway.
- The delegate action's own UI copy is reviewed against whatever "Read
  this third" item 2 concluded before this chunk ships -- a naming
  mismatch here is a real, user-facing correctness bug (promising a
  capability that doesn't exist), not a cosmetic detail.
```

---

## OC.15 — Outstanding orders & one-click payout

```
Item 5. "One-click payout" must go through exactly the same path every
other payout does -- this is a UI convenience over the real dispatch flow,
never a bypass of screening, energy reservation, or S1 approval.

BUILD internal/httpapi/payouts.go
  GET /payouts/outstanding
    - Lists orders in `screened` (ready to dispatch but not yet dispatched
      -- confirm whether any real orders sit in this state for a
      meaningful duration, or whether C5's own orchestrate loop already
      picks these up so fast this list is normally empty; if normally
      empty, say so in the UI rather than implying a queue that doesn't
      really exist) and orders in `dispatching` with a FAILED or stalled
      latest_attempt_status.
  POST /payouts/{external_id}/dispatch
    - Form: confirm (checkbox). Calls DispatcherClient's real
      POST /v1/dispatch -- the same call C5's own orchestrate loop makes,
      with the same Idempotency-Key convention (invariant 2). If this
      order's own payout crosses S1's approval threshold, the operator
      sees PENDING and the same OC.7 queue as any other payout -- "one
      click" means one click to START the real flow, not one click to
      skip a control that exists everywhere else in this system.

ACCEPTANCE
- Dispatching an order already in a terminal state (settled, refunded)
  is rejected with the real error dispatcher's own API returns, not a
  console-invented message.
- A dispatch that crosses the approval threshold in a test fixture shows
  PENDING and requires the same 2-of-N approval OC.11's sweep action does
  -- one shared test helper proving this invariant holds for both actions,
  since they both terminate in the same S1 flow.
```

---

## OC.16 — Ledger deep view: journal, trial balance, manual reversal & reorg

```
Closes "Read this fourth"'s first named gap. This chunk carries the
highest blast radius of anything in this document — a manual entry
reversal or a forced order transition writes directly against the
system of record — treat every acceptance criterion below as load-bearing,
not boilerplate.

BUILD internal/httpapi/ledger_admin.go
  GET /ledger/accounts, GET /ledger/balances
    - Thin wrap of GET /v1/accounts and GET /v1/balances -- this is the
      literal "overall balance of each part of the product" from item 1,
      not yet built anywhere. Render grouped by account-code prefix
      (asset:/liability:/revenue:/expense:/position:), matching the chart
      of accounts' own grouping in c1-ledger-build-prompts.md §A rather
      than a flat alphabetical list.
  GET /ledger/entries, GET /ledger/entries/{id}
    - Thin wrap of GET /v1/entries (list, paginated per C1's own real
      pagination shape) and GET /v1/entries/{id} (full line detail).
  POST /ledger/entries/{id}/reversal
    - Form requires a reason (free text, required, included in the audit
      log entry verbatim) and a second, explicit confirm step (type the
      entry id to confirm -- not just a checkbox, given the severity).
      Calls POST /v1/entries/{id}/reversal with Idempotency-Key per
      invariant 2. Shows C1's own real rejection if the entry is already
      reversed or reversal is otherwise illegal (C1.6's own "reversal of
      a reversal is rejected outright" -- the console must show that
      real error, never retry or paper over it).
  POST /ledger/orders/{external_id}/reorg
    - Thin wrap of POST /v1/orders/{external_id}/reorg. Same confirm-by-
      typing-the-id pattern as reversal.
  GET /ledger/trial-balance, GET /ledger/reconciliation-snapshots
    - Thin wraps of GET /v1/trial-balance and GET /v1/reconciliation/
      snapshots -- read-only, no form needed.
  POST /ledger/orders/{external_id}/transitions  (the highest-risk action
                                                    in this entire panel)
    - Thin wrap of POST /v1/orders/{external_id}/transitions. C1's own
      transition table already rejects illegal pairs (53 of 64, per
      C1.5's exhaustive test) -- this route is not a bypass of that
      validation. It IS a bypass of every OTHER service's own business
      reason for making a transition (C5 normally drives screened ->
      dispatching only after a real reservation and signature exist; this
      button can post that transition with none of that having happened).
      Gate this action behind a second, distinct confirmation string
      the operator must type ("I understand this skips C2/C3/C4/C5's own
      checks") -- not the same generic confirm every other button uses --
      and log it to the audit trail at a visibly different severity level
      than every other write in this console.

ACCEPTANCE
- Balances/accounts view matches a fake GET /v1/balances response exactly,
  grouped correctly by account-code prefix.
- Reversal of an already-reversed entry shows C1's real rejection, not a
  generic error -- test against a fake returning that specific error code.
- The forced-transition route has its own, separate audit-log entry type
  (e.g. "DANGEROUS_MANUAL_TRANSITION") distinguishable from ordinary writes
  by anyone reviewing the log later without reading full JSON detail.
- A grep-based test confirms both reversal and forced-transition templates
  render a reason field and reject empty-string submission client- AND
  server-side (the server check is the one that matters; the client one
  is only a courtesy).
```

---

## OC.17 — Deposit watcher remainder: orphaned deposits, providers, retire

```
Closes the C2 routes no existing chunk (OC.4, OC.12) reaches.

BUILD internal/httpapi/watcher_admin.go
  GET /watcher/addresses  (list/search -- OC.12 only shows WATCHING/FUNDED
                            for the deposit board; this is the full list,
                            including RETIRED, for the wallet inventory
                            OC.11 links out to)
  POST /watcher/addresses/{order_id}/retire
    - Thin wrap of POST /v1/addresses/{order_id}/retire. Confirm step
      names what retiring actually does (per C2's own doc comment on that
      route -- don't paraphrase past what it says).
  GET /watcher/orphaned-deposits
  POST /watcher/orphaned-deposits/{id}/resolve
    - Form matches whatever real resolution options
      POST /v1/orphaned-deposits/{id}/resolve's body accepts -- confirm
      the real enum against C2's code rather than guessing.
  GET /watcher/providers
    - Thin wrap of GET /v1/system/providers -- read-only health/config
      view of the RPC provider pool.

ACCEPTANCE
- Orphaned-deposits list and resolve round-trip against a fake, same
  pattern as every prior chunk's acceptance criteria.
- Retire shows a real confirmation naming the actual effect, verified by
  a test asserting the rendered confirm copy contains language pulled
  from (or consistent with) C2's own route documentation, not invented.
```

---

## OC.18 — Screening remainder: rescreen flags, result invalidation, queue

```
Closes the C3 routes OC.6 and OC.13 don't reach -- OC.6 covers holds only,
OC.13 is read-only.

BUILD internal/httpapi/screening_admin.go
  GET /screening/rescreen-flags
  POST /screening/rescreen-flags/{id}/resolve
  POST /screening/results/{id}/invalidate
    - Confirm step states the real consequence of invalidating a
      screening result (does it force a re-screen? does it affect an
      already-`screened` order? -- verify against C3's own code, this
      document does not know and should not guess) before shipping the
      confirm copy.
  GET /screening/queue
    - Thin wrap of GET /v1/system/queue -- the discovery/re-screen loop
      depth this component's own README section already tracks
      internally; surfacing it closes a real blind spot ("is the
      screening pipeline keeping up") no other chunk shows.

ACCEPTANCE
- Same pattern as OC.17: round-trip tests against fakes for every list +
  action pair, confirm copy verified against the real route's own
  documented behavior rather than assumed.
```

---

## OC.19 — Gateway (C6) admin surface — the largest real gap

```
Per "Read this fourth": gateway has NO operator-facing route today, for
anything. This chunk is mostly backend work in gateway/ itself, not a thin
console wrap -- say so plainly rather than sizing it like the others.

BACKEND (gateway, not opsconsole) -- new routes, service-token
authenticated the same way every other cross-service call in this system
already is (never customer sk_live_/sk_test_ keys, which authenticate the
OTHER direction)
  GET  /v1/admin/api-keys                 list, by customer_id
  POST /v1/admin/api-keys                 issue (body: customer_id, mode
                                           live|test) -- returns the raw
                                           key exactly once, at creation,
                                           the same "shown once, never
                                           again" convention every real
                                           API-key system uses; gateway
                                           already stores only the hash,
                                           so this isn't a new exposure,
                                           it's the existing issuance
                                           moment finally getting a route
  POST /v1/admin/api-keys/{id}/revoke
  GET  /v1/admin/webhooks/deliveries?status=failed
                                           delivery attempts, retry count,
                                           last error -- whatever fields
                                           C6.6's own webhook-delivery
                                           table already tracks
  POST /v1/admin/webhooks/deliveries/{id}/redrive
                                           manually re-trigger one delivery
                                           outside its normal backoff
                                           schedule
  GET  /v1/admin/orders                   cross-customer order list --
                                           the customer-facing
                                           GET /v1/orders is scoped to the
                                           calling API key by design (per
                                           C6's own sandbox-isolation
                                           discipline); this is a
                                           deliberately separate,
                                           admin-scoped route, not a
                                           parameter that widens the
                                           customer route's own scope
  GET  /v1/admin/rate-limits               current bucket/quota state per
                                            customer, if C6.3's rate
                                            limiter tracks anything
                                            queryable -- if it's in-memory
                                            and per-instance with nothing
                                            queryable, say that plainly
                                            instead of building a fake
                                            aggregate view

  Confirm every field/table name above against gateway's own C6.6 (webhook
  delivery) and C6.1-ish (API key storage) code before implementing --
  this document is proposing the shape, not asserting it matches
  gateway's exact internal schema, which this review did not read line by
  line the way it read every service's own openapi.yaml.

BUILD internal/opclient/gateway.go  (the seventh client -- extends the six
                                     that already exist, same shape)
BUILD internal/httpapi/gateway_admin.go
  Thin wraps of all five routes above, same confirm/audit conventions as
  every other write chunk in this document. The issued-key display is
  the one screen in this entire panel that DOES render a secret on
  purpose (the fresh API key, once) -- this does not conflict with
  invariant 5's "no secret ever rendered" the way a private-key button
  would, because a customer API key is this system's own credential,
  created for the purpose of being handed to that customer, not
  extracted key material from a signing system whose entire design
  promises it never leaves KMS. Say this distinction explicitly in a
  code comment at the point it might look like the same rule being broken
  twice, so a future reader isn't left to wonder.

ACCEPTANCE
- Key issuance renders the raw key exactly once; a second load of the
  same page (or the key list view) never shows it again, only a
  masked/last-4 form plus its hash's own creation metadata.
- Revoke actually prevents that key from authenticating on the NEXT real
  gateway call in the integration suite, not just a flag flipped in a
  response body.
- Webhook redrive against a fake shows the new attempt recorded with an
  incremented attempt count, not a duplicate row.
- Cross-customer admin order list returns orders across more than one
  customer_id in a seeded fixture -- the test that would have caught this
  chunk being built as "just widen the existing customer route."
```

---

## Suggested sequencing

| Chunk | Depends on | Notes |
|---|---|---|
| OC.10 Order detail + list + alerts | OC.0–OC.9 (shipped) | No backend changes |
| OC.11 Wallets + sweep-via-S1 | OC.10's opclient usage patterns | Read "Read this second" first — this is the chunk that most needs it |
| OC.12 Deposit watch + manual confirm | New C2 route (build first, in `depositwatcher/`) | |
| OC.13 AML exposure view | OC.6 (holds queue, shipped) | No backend changes |
| OC.14 Broker operations | Confirm vendor balance/delegate capability first (may descope) | |
| OC.15 Outstanding orders / one-click payout | OC.11 (shares the S1-approval test helper) | |
| OC.16 Ledger deep view (journal/reversal/reorg/forced transitions) | OC.0–OC.9 | Highest blast radius — do not rush the confirm-string/audit-severity acceptance criteria |
| OC.17 Watcher remainder (orphaned deposits/providers/retire) | OC.4, OC.12 | |
| OC.18 Screening remainder (rescreen/invalidate/queue) | OC.6, OC.13 | |
| OC.19 Gateway (C6) admin surface | None of the above — independent, but largest in scope | New backend service work, not a thin wrap; size accordingly |

Do OC.11 early relative to the others — it's the chunk most likely to surface
a hard "no" from the real system (no export, no BSC signing), and every later
chunk that touches "sweep" or "payout" should already reflect that answer
rather than re-litigate it per-chunk. OC.16 and OC.19 can run in parallel with
everything else — neither depends on OC.10–OC.15 — but budget OC.19 as
real cross-service backend work (new gateway routes, a new client, a new
storage/query need for webhook-delivery and rate-limit state), not a chunk
the size of OC.17/OC.18.

---

## Coverage matrix — every real route, one home each

The acceptance artifact for "Read this fourth." A route with no chunk in the
right-hand column is a gap this document missed, not one it deliberately
deferred — deliberate deferrals (private-key export, automated BSC sweep,
wallet-to-wallet energy retargeting if vendors don't support it) are named
in prose above, not left blank here.

| Service | Route | Chunk |
|---|---|---|
| C1 Ledger | `GET/POST /v1/accounts` | OC.16 |
| C1 Ledger | `GET /v1/accounts/{code}/balance` | OC.16 |
| C1 Ledger | `GET /v1/balances` | OC.16 |
| C1 Ledger | `GET/POST /v1/entries`, `GET /v1/entries/{id}` | OC.16 |
| C1 Ledger | `POST /v1/entries/{id}/reversal` | OC.16 |
| C1 Ledger | `GET/POST /v1/orders`, `GET /v1/orders/{external_id}` | OC.10 |
| C1 Ledger | `POST /v1/orders/{external_id}/transitions` | OC.16 (dangerous — extra confirm) |
| C1 Ledger | `POST /v1/orders/{external_id}/reorg` | OC.16 |
| C1 Ledger | `GET /v1/reconciliation/snapshots` | OC.16 |
| C1 Ledger | `POST /v1/system/halt` | OC.3 (shipped) |
| C1 Ledger | `GET /v1/system/invariants` | OC.2 (shipped) |
| C1 Ledger | `GET /v1/trial-balance` | OC.16 |
| C2 Watcher | `GET/POST /v1/addresses`, `GET /v1/addresses/{order_id}` | OC.10, OC.12, OC.17 |
| C2 Watcher | `POST /v1/addresses/{order_id}/retire` | OC.17 |
| C2 Watcher | `POST /v1/addresses/{order_id}/confirm-deposit` | OC.12 (**new C2 route**) |
| C2 Watcher | `GET /v1/orphaned-deposits`, `POST .../resolve` | OC.17 |
| C2 Watcher | `GET/POST /v1/system/cursor` | OC.4 (shipped) |
| C2 Watcher | `GET /v1/system/invariants` | OC.2 (shipped) |
| C2 Watcher | `GET /v1/system/providers` | OC.17 |
| C3 Screening | `GET /v1/holds`, `POST .../release`, `POST .../reject` | OC.6 (shipped) |
| C3 Screening | `GET /v1/rescreen-flags`, `POST .../resolve` | OC.18 |
| C3 Screening | `GET /v1/screening-results`, `POST .../invalidate` | OC.13 (read), OC.18 (invalidate) |
| C3 Screening | `GET /v1/system/queue` | OC.18 |
| C3 Screening | `GET /v1/system/invariants` | OC.2 (shipped) |
| C4 Broker | `GET/POST /v1/reservations`, `GET /v1/reservations/{id}` | OC.5 (shipped) |
| C4 Broker | `POST /v1/reservations/{id}/reconcile` | OC.5 (shipped) |
| C4 Broker | `GET /v1/buffer` | OC.14 |
| C4 Broker | `GET/POST /v1/manual-fallback-events`, `.../resolve` | OC.5 (shipped) |
| C4 Broker | `GET /v1/system/prices` | OC.14 |
| C4 Broker | `GET /v1/system/invariants` | OC.2 (shipped) |
| C4 Broker | *per-vendor account balance* | **not available** — confirm vendor API support first (Read this third #2) |
| C4 Broker | *rent-to-specific-wallet, wallet-to-wallet delegate* | OC.14 (**new C4 routes**, scope depends on vendor capability) |
| C5 Dispatcher | `GET/POST /v1/dispatch`, `GET /v1/dispatch/{order_id}` | OC.10 (read), OC.15 (write) |
| C5 Dispatcher | `GET /v1/slots`, `POST /v1/slots/{id}/retire` | OC.6 (shipped) |
| C5 Dispatcher | `GET /v1/system/invariants` | OC.2 (shipped) |
| C5 Dispatcher | *sweep a slot's own balance* | OC.11 (**new**, via S1, not a raw key) |
| S1 | `GET/POST /v1/signing-requests`, `GET .../{id}` | OC.7 (shipped, list); OC.11 (sweep, new use) |
| S1 | `POST /v1/signing-requests/{id}/approve`, `.../reject` | OC.7 (shipped) |
| S1 | `GET /v1/slots/{id}/address` | OC.11 (internal use) |
| S1 | *raw private key / seed* | **does not exist, will not be built — "Read this second"** |
| C6 Gateway | `GET/POST /v1/orders`, `/v1/quotes`, `/v1/sandbox/orders` | customer-scoped, not admin — visible via C1 through OC.10 instead |
| C6 Gateway | *API key issuance/revocation* | OC.19 (**new C6 routes**) |
| C6 Gateway | *webhook delivery visibility/redrive* | OC.19 (**new C6 routes**) |
| C6 Gateway | *cross-customer admin order list* | OC.19 (**new C6 route**) |
| C6 Gateway | *rate-limit state* | OC.19 (**new C6 route**, if queryable at all) |

If a Claude Code session runs this document and finds a real route this
table missed, treat that as this document's own bug — add a row and a
chunk, the same standard every other build-prompts doc in this repo holds
itself to (see this file's own "Read this third" and `README.md`'s running
account of gaps found and closed elsewhere in the project).
