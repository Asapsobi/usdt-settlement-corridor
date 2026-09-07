# Energy broker: manual fallback runbook

For whoever is paged when C4 (the energy broker) can't get energy from any
of the three primary vendors on its own. You did not build this system;
this document assumes that.

## First: check current state

```sql
SELECT * FROM manual_fallback_events WHERE resolved_at IS NULL ORDER BY id;
```

Every row here is a real, currently-open condition -- not a blip that
already recovered (a recovered blip never gets a row at all; see "How
`manual_fallback_events` gets written" below). `reason` tells you which of
the two sections below applies. `order_id` is set if a specific customer
reservation request is what surfaced this; null means C4's own background
buffer-replenishment loop found it first, before any order needed the
energy yet.

There are only ever two reasons a row exists here:

- **`all_unhealthy`** -- every one of Tronsell, Netts, and CatFee is either
  erroring or has gone price-stale. This is an outage, not a cost problem.
- **`all_over_ceiling`** -- all three vendors answered with a real,
  current price, but every one of them is priced above the configured
  ceiling (`ceiling_sun_per_unit` in C4's own routing config). This is a
  market-wide price spike, not an outage -- the vendors are working fine,
  they're just asking more than this system is configured to pay
  (invariant 3: a price above ceiling is never paid, no exceptions, not
  even "just this once").

## How `manual_fallback_events` gets written

C4's own routing logic (`internal/routing.Router.SelectProvider`) checks
all three vendors before every energy acquisition, whether that's the
background buffer topping itself up or a live customer reservation
falling through to a direct vendor call. When it finds nothing safe to
select, it calls `OnFallbackTriggered`, which:

- Records exactly one row per **distinct, ongoing** condition -- if ten
  reservations all hit the same outage in the same five minutes, you get
  one row, not ten. A live reservation's own repeated retries (it keeps
  checking every couple of seconds until its own deadline) also collapse
  into that same one row.
- Logs an error-level structured log line (`"routing: MANUAL FALLBACK
  TRIGGERED"`) with the event id -- if you have log-based alerting wired
  up, alert on that line. (A real paging integration is a later addition;
  today, this line plus polling the query above is the alerting.)
- Never attempts anything on JustLendDAO itself. That's you, below.

C4 does **not** invent a way to make a stuck reservation succeed on its
own while an event is open. A live customer reservation that hit this
during its own attempt will report `failed` the moment its own deadline
passes, whether or not you've resolved the event by then. Resolving the
event doesn't retroactively rescue that one reservation -- it only means
the *next* reservation, or the buffer's own next replenishment tick, gets
a fresh shot at the three primary vendors again.

---

## `all_unhealthy` -- all three vendors are down or unreachable

**What it means.** `internal/pricing.Poller` marks a vendor unhealthy the
instant a price poll errors, or stale once its last known-good price ages
past the configured staleness window. All three hit that state at once.

**What to check.**
- Is this a real, simultaneous three-vendor outage, or is it actually a
  problem on C4's own end (network egress broken, API tokens expired
  across the board, a bad deploy)? Three genuinely independent vendors
  failing at literally the same moment is possible but should make you
  check your own side first.
- If it does look like a real vendor-side outage: check each vendor's own
  status page / support channel. `c1-scenario-catalog.md`'s own Part 3
  flags "two or three providers degraded simultaneously" as a scenario
  that "tips into terminal risk if it happens for long enough" -- this
  event existing at all means you are now living that scenario, not
  reading about it.
- Check how long the buffer can still cover demand without any
  replenishment: `internal/buffer`'s own `TargetLevel`/available-total
  comparison is what the next scheduled Replenish tick will see. A buffer
  still comfortably above target buys you time before any live
  reservation is forced onto the (currently blocked) slow path at all.

**What to do.**
1. If the buffer still has headroom and the outage looks short-lived,
   the least risky move is often to simply wait a few polling cycles
   (`DefaultReplenishInterval`, 60s) and see if a vendor recovers on its
   own -- resolve the event once it does (see below), no manual
   acquisition needed.
2. If the buffer is draining and reservations are actively failing, or
   the outage has already run long enough that "wait it out" no longer
   looks safe: acquire energy manually via **JustLendDAO's own web UI**
   (non-custodial, on-chain, no API integration exists in this codebase
   on purpose -- see "Read this second" in this component's own build
   spec) as a stopgap:
   - Freeze/stake the TRX needed for the shortfall directly through
     JustLendDAO's UI, using S1's own signing key for whichever address
     is doing the staking.
   - Delegate the resulting energy resource directly to the specific
     payout slot address(es) that need it right now -- check
     `energy_buffer`/open reservations for which `target_address` values
     are currently stuck, rather than delegating to C4's own staging
     address (there is no automated path that will pick that up and
     redistribute it).
   - This is deliberately a manual, on-chain, out-of-band action. Nothing
     in C4 will detect or record that you did this on its own -- you
     record it yourself, in the resolution note, in the next step.
3. Resolve the event once vendors recover, or once you've manually
   covered the immediate need:
   ```sql
   UPDATE manual_fallback_events
   SET resolved_at = now(), resolution = '<what you did, and why it is safe now>'
   WHERE id = <event id> AND resolved_at IS NULL;
   ```
   Write the resolution for the next person, not for yourself: which
   vendor(s) recovered and when, or exactly what you manually delegated
   and to which address, and the on-chain transaction hash if you staked
   anything. If the underlying vendor outage is still ongoing when the
   next trigger fires, a **new** event opens (this table does not treat a
   resolved-too-early event as still covering a recurrence) -- that's
   intentional, not a bug to work around by leaving events open
   indefinitely "just in case."

---

## `all_over_ceiling` -- vendors are up, but too expensive

**What it means.** All three vendors answered with a real, current quote,
and every single one is above the configured ceiling. Nobody is down;
the market price of TRON energy has spiked above what this system is
configured to pay.

**What to check.**
- What are the three vendors actually quoting right now, and by how much
  do they exceed the ceiling? (`price_observations`, most recent row per
  `provider_name`, or watch `internal/pricing`'s own poll logs.) A spike
  a few percent over ceiling is a very different conversation from one at
  3x.
- Is this a brief spike (network congestion, a single large buyer moving
  the market) or a sustained repricing (e.g. TRX itself moved sharply, or
  the whole energy-rental market re-based)? Check how long
  `price_observations` has shown all three over ceiling.
- Confirm the ceiling itself is still the right number. It should be a
  number someone with AML/compliance and margin authority actually signed
  off on (see "Read this third" in this component's own build spec) --
  this event firing is sometimes the market telling you the configured
  ceiling is stale, not that today is unusual.

**What to do.**
1. If it looks like a brief spike: the same "wait a few polling cycles"
   approach as `all_unhealthy` above is usually right -- prices that
   spiked briefly tend to come back down, and paying above ceiling to
   avoid a short delay is exactly the invariant this system exists to
   prevent you from doing under pressure.
2. If it's a sustained repricing and reservations are failing in volume:
   this is a **business decision, not an engineering one**. Raising the
   ceiling is a config change that needs the same sign-off invariant 3's
   own "Read this third" describes -- whoever owns margin and compliance,
   not whoever is on call, decides whether to pay more per payout.
   JustLendDAO (manual, same as above) is the other lever if staking your
   own TRX at the old margin is preferable to raising the ceiling.
3. Resolve the same way as `all_unhealthy` above, with a resolution note
   naming which of the two happened (ceiling raised, by whom and to what
   value; or prices recovered on their own; or manually covered via
   JustLendDAO).

---

## Notes for whoever extends this later

- There is currently no HTTP endpoint for listing or resolving these
  events -- direct SQL, as above, is the whole interface at MVP. If this
  ever gets an operator UI or a `/v1/fallback-events` endpoint, keep this
  runbook's own SQL as the documented fallback for when that UI itself is
  what's degraded.
- `internal/routing.KnownFallbackReasons` is the enforced source of truth
  for which reasons this document must cover -- a build-time test
  (`TestRunbookCoversEveryFallbackReason`) fails if a new reason is added
  in code without a matching section here.
