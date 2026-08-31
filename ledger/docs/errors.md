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
| 400 | `invalid_request` | The request itself is malformed or fails basic validation, unrelated to an amount. | Malformed JSON, an unknown field, a missing required field, a bad enum value, a missing `Idempotency-Key` header, `halt.ErrClearRequiresOperator`. |
| 422 | `invalid_entry` | The entry's *shape* is invalid for this operation, distinct from a balance or asset problem. | Fewer than two lines, a zero-amount line, an entry supplied to a transition that doesn't accept one, an entry omitted from a transition that requires one. |
| 401 | `unauthorized` | Missing or invalid bearer token. |  |

## Notes

- Every code above is asserted by a golden-file test in
  `internal/httpapi` — exact status, exact code string, per condition.
- `invalid_amount` covers both the JSON-level rejection (a JSON number
  where a decimal string was expected) and the domain-level rejection (a
  syntactically valid string that isn't a valid decimal, or has more
  decimal places than its asset allows, or overflows int64).
