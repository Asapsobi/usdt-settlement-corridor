# C3 screening reason codes

Stable, documented strings reported to C1 as the `reason` on a screening
transition (see §A of `docs/03-build/c3-screening-build-prompts.md`). One
code per condition, never reused for a different condition -- same
discipline C1 applies to its own error codes.

Defined in `internal/verdict/verdict.go`.

| Code | Meaning | Produced by |
|---|---|---|
| `screening_pass` | Provider returned a clean result within the timeout, below both thresholds, not vendor-flagged. | `verdict.Classify` |
| `screening_hold_flagged` | The provider explicitly flagged the address (`Verdict.Flagged == true`), **or** its risk score is at or above `Thresholds.FlaggedAtOrAbove` even without an explicit vendor flag. An explicit `Flagged=true` always wins regardless of score. | `verdict.Classify` |
| `screening_hold_ambiguous` | Risk score falls in the configured grey band: at or above `Thresholds.PassBelow` but below `Thresholds.FlaggedAtOrAbove`, and the vendor did not explicitly flag it. | `verdict.Classify` |
| `screening_hold_unavailable` | The vendor call timed out or errored and the fail-closed outage policy fired (see "Read this third" in the C3 build spec). There is no `Verdict` to classify and no backing `screening_results` row. | `verdict.Unavailable` |
| `screening_hold_stale_cache_invalidated` | An operator invalidated a prior cached pass (`internal/cache.Invalidate`) and the order is re-held pending a fresh screen. | C3.7 (not yet built) |

## Threshold boundary semantics

`Thresholds.PassBelow` and `Thresholds.FlaggedAtOrAbove` are both
inclusive on the hold side: a score *exactly equal* to either cutoff is
treated as "far enough into the band to hold," never "not quite there
yet." Concretely, for a non-flagged verdict:

```
score <  PassBelow            -> Pass
PassBelow <= score < FlaggedAtOrAbove -> Hold, screening_hold_ambiguous
score >= FlaggedAtOrAbove     -> Hold, screening_hold_flagged
```

`DefaultThresholds` (`PassBelow: 0.5`, `FlaggedAtOrAbove: 0.85`) is a
placeholder, not a calibrated policy -- no screening vendor is chosen yet
(§B of the build spec), so nobody has seen a real score distribution to
tune these against. Revisit once one is.
