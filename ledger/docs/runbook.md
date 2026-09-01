# Ledger runbook

For whoever is on call when the ledger halts. You did not build this system;
this document assumes that.

## First: check current state

```
GET /v1/system/halt         -> {"halted": true, "reason": "..."}
GET /v1/system/invariants   -> live trial balance, cache-verification
                                result, corridor position vs ceiling,
                                reconciler lag, halt state
```

`halt_log` has the full history (every set and every clear, with actor and
detail), not just the current state:

```sql
SELECT * FROM halt_log ORDER BY id DESC LIMIT 20;
```

The `detail` column on the most recent `'set'` row is usually the fastest way
to see *why*, in structured form, before reading any of the sections below.

## How a halt is cleared, in general

There is no automatic clearing anywhere in this system. Every halt stays
halted until an operator explicitly clears it:

```
POST /v1/system/halt
{"action": "clear", "note": "<what you checked, what you found, why it's safe to resume>"}
```

Both an operator identity (the bearer token's actor) and a non-empty `note`
are required -- the API rejects a clear without one. The note is permanent,
in `halt_log`, read by the next person who ends up here. Write it for them,
not for yourself.

**Before you clear anything**, read the section below for the specific
reason shown by `GET /v1/system/halt`. Some reasons need real investigation
before it's safe to resume; none of them are safe to clear just because the
API will let you.

While halted, every write that would post a new journal entry or transition
an order through a halt-blocked step is rejected with `system_halted`.
Reads, and the four transitions that don't touch money movement, are not
blocked. Ingesting a reconciliation snapshot is also never blocked, even
while already halted (that's how you'd learn a second problem exists while
still investigating a first one).

---

## POST_SETTLEMENT_REORG

**What it means.** A BEP20 deposit that had already reached `dispatching` or
`settled` was reported as reorged out. The payout already happened (or is in
flight) and cannot be undone; the deposit that was supposed to fund it just
stopped existing. This is a real, permanent loss, not a transient error.

**What the system already did, automatically, before halting:**
- Reversed the original deposit entry (the inbound side only -- the payout
  entry is untouched, because it genuinely happened).
- Posted a loss entry: debit `expense:loss:reorg`, credit
  `position:corridor:<asset>`, for the order's `amount_out`.
- Halted.

The accounting is already correct at the moment you read this. You are not
here to fix a broken ledger; you are here to decide whether it's safe to
keep operating.

**What to check.** The halt_log `detail` for this reason has `order_id`,
`order_external_id`, `original_entry_key`, `loss_asset`, `loss_units`.

- Confirm `loss_units` matches that order's `amount_out` (`GET
  /v1/orders/{external_id}`) -- if it doesn't, something else is wrong and
  this section doesn't apply; escalate.
- Check whether this is an isolated event or part of a pattern (multiple
  `POST_SETTLEMENT_REORG` rows in `halt_log` in a short window). One
  reorg at typical confirmation depth is bad luck. Several in a row usually
  means the deposit-confirmation threshold (15 confirmations, currently) is
  too shallow for current chain conditions, or something upstream is
  reporting confirmations incorrectly -- that's a config/upstream problem,
  not something this halt clears on its own.
- Confirm `expense:loss:reorg`'s balance (`GET
  /v1/accounts/expense:loss:reorg/balance`) reflects this loss and any
  prior ones -- this account should equal the running total of every
  reorg loss ever booked, so a random jump not explained by `halt_log`
  is itself worth investigating before clearing.

**How to clear safely.** Once you've confirmed the loss is legitimate,
correctly sized, and not part of an active pattern that needs a separate
fix first: clear normally, with a note naming the order and confirming the
loss amount was checked.

---

## TRIAL_BALANCE_BROKEN

**What it means.** The reconciler's self-check summed every `journal_lines`
row directly (not through the `account_balances` cache) and found a
nonzero total for some asset. This is the most severe halt reason in this
system: the core double-entry identity -- every entry sums to zero, per
asset -- does not hold. A Postgres deferred constraint trigger is supposed
to make this structurally impossible at commit time. If this fires, that
protection was somehow bypassed or defeated.

**What to check.**
- The halt_log `detail` has `asset` and `total_units` -- which asset is
  unbalanced, and by how much (in that asset's minor units).
- Find the offending entry. Since a healthy entry always sums to zero
  per asset, group `journal_lines` by `entry_id` and asset and look for
  any group that doesn't sum to zero:
  ```sql
  SELECT entry_id, asset, sum(amount_units)
  FROM journal_lines
  GROUP BY entry_id, asset
  HAVING sum(amount_units) <> 0;
  ```
- Once you have the entry, ask how it got there. In rough order of
  likelihood: a migration that altered or dropped the deferred constraint
  trigger (`migrations/0003_journal.sql`) without anyone noticing; a
  manual `UPDATE`/`INSERT` against `journal_lines` outside `journal.Post`/
  `journal.Reverse` (both those functions cannot produce an unbalanced
  entry validated as such -- `validate` in `internal/journal` rejects it
  before anything is written); or a role/connection with elevated
  privileges that bypassed the constraint entirely.
- Confirm the trigger itself still exists and is still a constraint
  trigger (`\d+ journal_lines` in psql, or query `pg_trigger`).

**How to clear safely.** Do not clear until you've found the specific
unbalanced entry and understand how it happened. There is no automated
repair for this -- the fix depends entirely on the cause: if the trigger
was dropped, restore it (and figure out why the deferred check didn't
already catch this before you noticed); if a bad entry got in through
some other path, you likely need to book a correcting entry by hand
(reversal only works cleanly on an entry that was itself balanced to
start with, which this one is not) with someone who understands the
specific discrepancy. This is a "get another engineer" halt, not a
solo-operator one.

---

## CACHE_DIVERGENCE

**What it means.** `journal.VerifyBalances` compared `account_balances`
(the running-total cache each write updates) against a fresh sum over
`journal_lines` for every account, and found at least one account where
they disagree. `journal_lines` is the source of truth; `account_balances`
is a cache that's supposed to always match it exactly -- there is no
window where they're allowed to differ, by design (both are updated in the
same transaction, same commit, on every write).

**What to check.**
- The halt_log `detail.discrepancies` array lists every mismatched account:
  `account_code`, `asset`, `cached_units` (what `account_balances` says),
  `computed_units` (what summing `journal_lines` says).
- Recompute independently to confirm:
  ```sql
  SELECT account_id, asset, sum(amount_units)
  FROM journal_lines
  WHERE account_id = <id> AND asset = '<asset>'
  GROUP BY account_id, asset;
  ```
  compared against `SELECT balance_units FROM account_balances WHERE
  account_id = <id> AND asset = '<asset>'`.
- This almost always means one of the two tables was written to outside
  the normal `journal.Post`/`journal.Reverse` path (which always updates
  both in the same transaction) -- a manual fix-up, a partially-applied
  migration, or a bug in `insertLinesAndApplyBalances` itself. Check for
  any manual SQL run against either table recently.

**How to clear safely.** `journal_lines` is authoritative -- never "fix"
this by changing history in `journal_lines` to match the cache. The
correct repair is almost always recomputing the cached row from
`journal_lines` for each affected account:
```sql
UPDATE account_balances
SET balance_units = (
    SELECT COALESCE(sum(amount_units), 0)
    FROM journal_lines
    WHERE journal_lines.account_id = account_balances.account_id
      AND journal_lines.asset = account_balances.asset
),
updated_at = now()
WHERE account_id = <id> AND asset = '<asset>';
```
Re-check `GET /v1/system/invariants` (`cache_ok` should read `true`, with
an empty `discrepancies` list) before clearing.

---

## BALANCE_DRIFT

**What it means.** Someone (a chain watcher, an operator) posted a
reconciliation snapshot via `POST /v1/reconciliation/snapshots` reporting
what an account actually holds on-chain, and it disagreed with what the
ledger's own history says that account should hold *as of the snapshot's
stated watermark* -- by more than the configured tolerance. Every USDT
asset's tolerance is hardcoded to zero (any drift is real, since USDT is
exact); only TRX has a configurable nonzero tolerance
(`RECON_TRX_TOLERANCE_UNITS`), to absorb energy/bandwidth rounding.

**What to check.** The halt_log `detail` has `account_code`, `asset`,
`observed_units` (what the chain says), `ledger_units` (what the ledger
believed, at that watermark), `drift_units`, `watermark`, `chain_ref`.

- Confirm the snapshot itself is trustworthy: right account, right chain,
  watermark actually corresponds to `chain_ref`. A stale or misattributed
  snapshot produces a false BALANCE_DRIFT.
- If the snapshot is trustworthy, figure out which side is wrong:
  - Ledger is behind chain (`observed_units` more than expected): a
    deposit or inbound transfer the ledger never recorded -- look for an
    on-chain transaction to this account with no corresponding
    `journal_lines` row.
  - Ledger is ahead of chain (`observed_units` less than expected): the
    ledger recorded money that isn't actually there -- a payout that
    didn't confirm, a reorg that reversed something and wasn't reported
    through the normal `POST_SETTLEMENT_REORG` path, or a bug.
- `GET /v1/accounts/{code}/balance?as_of_entry_id={watermark}` reproduces
  exactly the `ledger_units` figure in the detail, for cross-checking.

**How to clear safely.** Do not clear until the drift is explained. If the
ledger was missing a real event, post the correcting entry for it first
(through the normal API, with its own idempotency key) and confirm a
fresh snapshot at a newer watermark comes back within tolerance. If the
ledger recorded something that turns out not to have happened, treat that
with the same seriousness as TRIAL_BALANCE_BROKEN -- get a second
engineer before deciding how to correct it.

---

## Operator-set halts

`POST /v1/system/halt` with `action: "set"` lets an operator halt manually,
with a free-text `reason` and optional `detail` -- this is not one of the
four reasons above, and won't match anything grep-checked against this
document. It exists for "I don't trust what I'm seeing, stop everything
while I look" situations that don't fit an automated check. Read the
`reason` and `detail` the operator who set it wrote, and, if you didn't set
it yourself, try to reach them before clearing.

---

## Interpreting a non-zero `position:corridor`

**This is not a halt condition**, and does not appear anywhere above --
deliberately. `position:corridor:<asset>` is described in detail in
[`accounts.md`](accounts.md); in short, it's the honest record of inventory
currently owed on one chain but not yet replenished from the other, between
the conversion entry (E2) and the treasury rebalance that closes it (E5,
posted by the treasury runbook -- a different document, owned by whoever
runs that loop, not this one).

A nonzero, even a large and growing, `position:corridor` with an otherwise
healthy `GET /v1/system/invariants` (`trial_balance_ok: true`, `cache_ok:
true`) means the treasury rebalance loop is *behind*, not that anything is
*broken*. That distinction is the entire reason C1's reconciler only alerts
on this (a `slog.Warn` line, `"position:corridor exceeds configured
ceiling"`) and never halts.

**What to check:**
- `GET /v1/system/invariants` includes `corridor_position`, one entry per
  asset with a configured ceiling
  (`RECON_CORRIDOR_CEILING_BEP20_UNITS`/`RECON_CORRIDOR_CEILING_TRC20_UNITS`),
  each showing the current position, its magnitude's ceiling, and whether
  it's currently over.
- If `over_ceiling` is true: this means the rebalance loop has fallen
  further behind than expected under normal operation. It is not this
  runbook's job to fix that -- escalate to whoever owns the treasury
  runbook. Your job here is confirming the ledger itself is healthy
  (`trial_balance_ok`/`cache_ok` both true) while that happens, since a
  slow rebalance combined with an actually-broken ledger is a much more
  urgent problem than either alone.
- After a burst of orders settle and treasury catches up, `position:corridor`
  moving back toward zero per asset is the expected, healthy pattern -- not
  something that needs to reach exactly zero to be considered fine.
