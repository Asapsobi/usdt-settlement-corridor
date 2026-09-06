# Error codes

Every error response from the HTTP API (`internal/httpapi`) has this shape:

```json
{"error": {"code": "unbalanced_entry", "message": "human-readable, may change"}}
```

`code` is stable: never reused for a different condition. `message` is not
part of the contract and may be reworded at any time — programmatic callers
must key off `code`, never `message`.

## The build spec's table (verbatim)

| Status | Code | Meaning |
|---|---|---|
| 409 | `idempotency_conflict` | Same `Idempotency-Key`, different payload. |
| 422 | `unbalanced_entry` | Entry does not sum to zero for at least one asset. |
| 422 | `asset_mismatch` | A line's asset doesn't match its account's fixed asset. |
| 423 | `system_halted` | The ledger is halted and this operation is halt-blocked. |
| 409 | `illegal_transition` | No such state transition is legal from the order's current state. |
| 409 | `version_conflict` | `expected_version` didn't match the order's current version (optimistic concurrency lost). |
| 404 | `account_not_found` | No account exists with that code. |
| 404 | `order_not_found` | No order exists with that id / external_id. |
| 400 | `invalid_amount` | An amount was not a valid decimal string for its asset — including a JSON *number* in an amount field, which is always rejected. |
| 500 | `internal` | Unclassified internal error. |

## Extensions

The table above doesn't cover every condition an HTTP boundary must
handle. These three are additions this chunk introduces, each mapped from
real domain errors lower layers can return:

| Status | Code | Meaning | Example causes |
|---|---|---|---|
| 400 | `invalid_request` | The request itself is malformed or fails basic validation, unrelated to an amount. | Malformed JSON, an unknown field, a missing required field, a bad enum value, a missing `Idempotency-Key` header, `halt.ErrClearRequiresOperator`, `journal.ErrInvalidReverseParams`, `orders.ErrInvalidParams` (e.g. a malformed `GET /v1/orders` cursor, or `sender_address` supplied on a transition that isn't into `funded`), a missing/unknown `state` or non-positive `limit` on `GET /v1/orders`. |
| 422 | `invalid_entry` | The entry's *shape* is invalid for this operation, distinct from a balance or asset problem. | Fewer than two lines, a zero-amount line, an entry supplied to a transition that doesn't accept one, an entry omitted from a transition that requires one, both `entry` and `entry_id` supplied to one transition. |
| 401 | `unauthorized` | Missing or invalid bearer token. |  |

## C1.11 — the reversal surface

These four became reachable over HTTP when `POST /v1/entries/{id}/reversal`
and `POST /v1/orders/{external_id}/reorg` were added. Each maps from one
domain sentinel C1.6 already defined.

| Status | Code | Meaning | Raised by |
|---|---|---|---|
| 404 | `entry_not_found` | No journal entry exists with that id, or under that idempotency key. | `journal.ErrEntryNotFound` |
| 409 | `already_reversed` | The entry already has a reversal. An entry may be reversed **at most once** — enforced by a UNIQUE constraint on `journal_entries.reversal_of`, not only in Go. | `journal.ErrAlreadyReversed` |
| 422 | `cannot_reverse_a_reversal` | The entry named is itself a reversal. | `journal.ErrCannotReverseAReversal` |
| 409 | `unexpected_state` | A deposit reorg was reported for an order in a state neither reorg scenario is defined for (anything but `funded`, `dispatching`, `settled`). Deliberately an error rather than a best-guess branch. | `orders.ErrUnexpectedState` |

### `already_reversed` carries the existing reversal

`Reverse` is deliberately **not** idempotent: a second call is a hard
error, never a silent replay, because "at most one reversal per entry" is
a structural invariant rather than a request that happens to repeat.

Over HTTP that leaves a caller whose first request timed out unable to
tell *"I already did this"* from *"someone else did this"* — both look
identical on retry. So the 409 body carries the reversal that actually
exists alongside the usual envelope:

```json
{
  "error": {"code": "already_reversed", "message": "..."},
  "existing_reversal": {"id": 4210, "reversal_of": 4187, "...": "..."}
}
```

The invariant is unchanged — the second call still writes nothing and
still reports a conflict. `existing_reversal` is omitted if it cannot be
read back; the status and `code` are the contract, that field is a
convenience. Programmatic callers must still key off `code`.

## Notes

- Every code above is asserted by a golden-file test in
  `internal/httpapi` — exact status, exact code string, per condition.
- `invalid_amount` covers both the JSON-level rejection (a JSON number
  where a decimal string was expected) and the domain-level rejection (a
  syntactically valid string that isn't a valid decimal, or has more
  decimal places than its asset allows, or overflows int64).
