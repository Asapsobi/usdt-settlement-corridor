# Chart of accounts

## The accounts

| Code | Type | Asset | Holds |
|---|---|---|---|
| `asset:bsc:deposit:<order_id>` | ASSET | USDT_BEP20 | Per-order deposit address balance |
| `asset:tron:slot:<slot_id>` | ASSET | USDT_TRC20 | One of six payout slots ($50k / 5,000 tx cap) |
| `asset:tron:energy_wallet` | ASSET | TRX | TRX for energy and bandwidth |
| `asset:cex:<venue>` | ASSET | one per asset | Rebalancing venue balance -- a separate account per asset |
| `liability:customer:<id>` | LIABILITY | one per asset | What we owe, per asset -- a separate account per asset |
| `position:corridor:<asset>` | POSITION | one per asset | Inventory position between chains -- see [Position: corridor](#position-corridor) |
| `revenue:fee` | REVENUE | USDT_TRC20 | bp fee |
| `revenue:network_fee` | REVENUE | USDT_TRC20 | Per-payout network fee |
| `expense:energy` | EXPENSE | TRX | Energy rental |
| `expense:bandwidth` | EXPENSE | TRX | Bandwidth |
| `expense:gas` | EXPENSE | BNB | BSC gas |
| `expense:rebalance` | EXPENSE | USDT_TRC20 | Cost of moving inventory |
| `expense:loss:reorg` | EXPENSE | USDT_TRC20 | Post-settlement reorg loss |
| `expense:loss:freeze` | EXPENSE | USDT_TRC20 | Frozen slot loss |
| `equity:opening:<asset>` | EQUITY | one per asset | Opening balances only -- a separate account per asset |

An account has exactly one asset for its whole life. `position:corridor` and
`equity:opening` read as "both"/"all" above only because each is really a
*family* of accounts, one per asset it needs (`position:corridor:USDT_BEP20`,
`position:corridor:USDT_TRC20`, ...) -- the asset is a trailing code segment,
the same way `<order_id>`/`<slot_id>`/`<venue>` are. `internal/accounts.Seed`
creates every fixed-code account up front; the variable-code ones
(`asset:bsc:deposit:<order_id>`, `asset:tron:slot:<slot_id>`,
`liability:customer:<id>`, `asset:cex:<venue>`) are created on demand as
orders, slots, customers, and venues come into existence.

## Position: corridor

BEP20 and TRC20 USDT are separate assets. A conversion between them is not a
transfer -- it's an exchange, and between the moment that exchange is booked
(E2 below) and the moment treasury actually moves the inventory to back it
(E5, posted by the treasury runbook, not by this system), `position:corridor`
is the ledger being honest about a real, temporary fact: money has been
promised out on one chain, backed by money that is still sitting on the
other.

A nonzero, even large, `position:corridor` is not a problem by itself --
see [`runbook.md`](runbook.md#interpreting-a-non-zero-position-corridor) for
how to tell "slow" from "broken." `GET /v1/system/invariants` reports the
current position for every asset with a configured ceiling.

## Entry types

Every entry this system posts carries an `entry_type`. These are the five
this system (C1) actually produces on its own -- not `POST /v1/entries`'
caller-supplied field, which future components can set to anything, but
what C1's own internal logic (`internal/orders`, and the shape every real
caller is expected to follow) actually emits:

| entry_type | Posted by | When |
|---|---|---|
| `deposit_final` | Caller of `quoted -> funded` | A BEP20 deposit reaches confirmation depth |
| `conversion` | Caller of `screened -> dispatching` | The exchange between assets is booked |
| `payout_settled` | Caller of `dispatching -> settled` | A TRC20 payout reaches SR finality |
| `reversal` | `internal/journal.Reverse` | Any entry is reversed -- always this literal type, regardless of what it's reversing |
| `reorg_loss` | `internal/orders.HandleDepositReorg` (scenario B) | A deposit reorgs out *after* its payout already happened |

`E4` (energy cost) and `E5` (treasury rebalance) from the worked example
below are posted by C4 and the treasury runbook respectively, not by C1 --
they're shown here because they touch accounts C1 owns, not because C1
produces them.

---

## Worked examples

All examples use the same $3,000 Standard order: fee 25 bp = $7.50, network
fee $1.80, payout $2,990.70. Every line's `amount` is a signed decimal
string in that line's own asset's minor units (6dp for USDT); the ledger-wide
convention is positive = debit, negative = credit, and every asset within
one entry balances to zero independently.

### `deposit_final` (`quoted -> funded`)

The deposit address received the funds; we now owe the customer.

```
DR  asset:bsc:deposit:1042     +3000.000000  USDT_BEP20
CR  liability:customer:acme    -3000.000000  USDT_BEP20
                                ─────────────  BEP20 sums to 0 ✓
```

Idempotency key convention: `<producer>:deposit_final:<tx_hash>:<log_index>`
-- the natural id comes from the chain event being reacted to, so a retried
call reconstructs the same key.

### `conversion` (`screened -> dispatching`)

One entry, two assets. We stop owing BEP20 and start owing TRC20, net of
fees.

```
DR  liability:customer:acme    +3000.000000  USDT_BEP20    we no longer owe BEP20
CR  position:corridor:USDT_BEP20  -3000.000000  USDT_BEP20
                                ─────────────  BEP20 sums to 0 ✓
DR  position:corridor:USDT_TRC20  +3000.000000  USDT_TRC20
CR  liability:customer:acme    -2990.700000  USDT_TRC20    we now owe TRC20
CR  revenue:fee                   -7.500000  USDT_TRC20
CR  revenue:network_fee           -1.800000  USDT_TRC20
                                ─────────────  TRC20 sums to 0 ✓
```

### `payout_settled` (`dispatching -> settled`)

The payout reached SR finality on Tron; the customer liability is cleared
from whichever slot paid it.

```
DR  liability:customer:acme    +2990.700000  USDT_TRC20
CR  asset:tron:slot:3          -2990.700000  USDT_TRC20
```

### `reversal`

Negates every line of the entry it reverses -- same accounts, same assets,
opposite sign -- and is always `entry_type: "reversal"` regardless of what
kind of entry it's reversing. `reversal_of` links back to the original;
`internal/journal.Reverse` refuses to reverse an entry that's already been
reversed, or to reverse a reversal itself.

Reversing the `deposit_final` example above (say, screening later rejects
the order and the deposit is refunded):

```
DR  liability:customer:acme    +3000.000000  USDT_BEP20
CR  asset:bsc:deposit:1042     -3000.000000  USDT_BEP20
                                ─────────────  BEP20 sums to 0 ✓
```

Idempotency key convention: deterministic, `ledger:reverse:` + the original
entry's own idempotency key -- a retried reversal call reconstructs the
exact same key rather than minting a fresh one, the same replay guarantee
every other entry in this system has.

### `reorg_loss`

Posted only by `internal/orders.HandleDepositReorg`'s scenario B: the
deposit that funded an order is reported reorged out, but the payout
already happened (order was `dispatching` or `settled`) and cannot be
undone. The inbound side is reversed (a separate `reversal` entry, not shown
again here); this entry books the resulting gap as a realized, permanent
loss and is what triggers the `POST_SETTLEMENT_REORG` halt (see
[`runbook.md`](runbook.md#post_settlement_reorg)).

```
DR  expense:loss:reorg            +2990.700000  USDT_TRC20
CR  position:corridor:USDT_TRC20  -2990.700000  USDT_TRC20
                                ─────────────  TRC20 sums to 0 ✓
```

The amount is always the order's `amount_out` -- the TRC20 that genuinely
left and will now never be backed by the BEP20 side, since that BEP20 side
just evaporated. Idempotency key convention: `ledger:reorg_loss:<order_id>`
-- deterministic and order-scoped, so a retried call for the same order
reaches the same key.

---

## Not produced by C1 (shown for reference only)

### `E4` -- energy cost (posted independently by C4, in TRX)

```
DR  expense:energy                +2.438000  TRX          $0.829 at TRX $0.34
CR  asset:tron:energy_wallet      -2.438000  TRX
```

### `E5` -- treasury rebalance closes the position (posted by the treasury runbook)

```
CR  asset:cex:binance           -3000.000000  USDT_BEP20
DR  position:corridor:USDT_BEP20  +3000.000000  USDT_BEP20
                                ─────────────  BEP20 sums to 0 ✓
CR  position:corridor:USDT_TRC20  -3000.000000  USDT_TRC20
DR  asset:tron:slot:3           +2999.550000  USDT_TRC20
DR  expense:rebalance              +0.450000  USDT_TRC20   1.5 bp
                                ─────────────  TRC20 sums to 0 ✓
```
