// Package verdict turns a raw provider.Verdict into the decision C3
// actually reports to C1: Pass or Hold, with a stable reason code.
//
// Classify is a pure function -- no chain access, no cache, no HTTP call
// to C1 or to a vendor. Every impure part (calling the provider, hitting
// internal/cache, calling C1) lives in the packages that call this one
// (C3.4 and later). This package never produces Reject: per §B of
// docs/03-build/c3-screening-build-prompts.md, reject-on-review is only
// ever reached manually, via C3.6's hold-resolution path, never
// automatically from a provider's verdict.
package verdict

import "screening/internal/provider"

// Reason codes are stable, documented strings (see docs/reason-codes.md)
// -- one per condition, never reused, same discipline as C1's own error
// codes.
const (
	ReasonPass                      = "screening_pass"
	ReasonHoldFlagged               = "screening_hold_flagged"
	ReasonHoldAmbiguous             = "screening_hold_ambiguous"
	ReasonHoldUnavailable           = "screening_hold_unavailable"
	ReasonHoldStaleCacheInvalidated = "screening_hold_stale_cache_invalidated"
	// ReasonPassVendorUnavailable is C3.5's fail-open outcome: every
	// retry against the vendor was exhausted, and the configured outage
	// policy is FailOpen, so the order is passed through anyway. Never
	// just ReasonPass -- this string exists specifically so the origin
	// is grep-able and auditable after the fact, distinguishable from an
	// actual clean vendor result.
	ReasonPassVendorUnavailable = "screening_pass_vendor_unavailable"
)

// Classification is deliberately a closed, two-value set. There is no
// Reject constant -- see the package doc comment.
type Classification int

const (
	Pass Classification = iota
	Hold
)

func (c Classification) String() string {
	switch c {
	case Pass:
		return "pass"
	case Hold:
		return "hold"
	default:
		return "unknown"
	}
}

// Decision is what C3.4 reports to C1: a classification and the reason
// code behind it.
type Decision struct {
	Classification Classification
	ReasonCode     string
	// ScreeningResultID is the FK to screening_results for the row this
	// decision was classified from. Classify itself never sets this --
	// it has no cache access -- the caller fills it in once the Verdict
	// has actually been persisted (internal/cache.Put's returned row, or
	// looked up via internal/cache.Get). It stays 0 for a decision with
	// no backing result row at all, e.g. Unavailable's fail-closed hold.
	ScreeningResultID int64
}

// Thresholds are the risk-score cutoffs between the three bands Classify
// can land a non-manual verdict in: pass, ambiguous, and (score-driven)
// flagged. Config, not a hardcoded constant -- see this package's own
// build spec: nobody has seen a real vendor's actual score distribution
// yet (no vendor is chosen, §B), so these are the part of this chunk
// most likely to need tuning once one is.
type Thresholds struct {
	// PassBelow: a risk score strictly below this is Pass. At or above,
	// the verdict enters hold territory (ambiguous or flagged, below).
	PassBelow float64
	// FlaggedAtOrAbove: a risk score at or above this is treated the
	// same as the vendor's own Flagged=true -- reason ReasonHoldFlagged
	// -- even if the vendor itself did not set Flagged. Between
	// PassBelow and this cutoff is the ambiguous grey band
	// (ReasonHoldAmbiguous).
	FlaggedAtOrAbove float64
}

// DefaultThresholds is a conservative placeholder -- 0.5 / 0.85 -- not a
// real, vendor-calibrated policy. Loud on purpose: see Thresholds' own
// doc comment and docs/reason-codes.md.
var DefaultThresholds = Thresholds{
	PassBelow:        0.5,
	FlaggedAtOrAbove: 0.85,
}

// Classify turns v into a Decision under thresholds. Pure: the same
// (v, thresholds) always yields the same Decision.
//
// Boundary semantics (deliberately not left to floating-point luck): a
// score is Pass only if strictly below PassBelow. A score exactly equal
// to PassBelow, or exactly equal to FlaggedAtOrAbove, falls on the
// hold side of that boundary -- >= is "far enough into the band to
// hold," not "not quite there yet."
//
// v.Flagged, if true, always classifies Hold/ReasonHoldFlagged
// regardless of RiskScore, including a score of 0.0 -- an explicit
// vendor flag overrides the numeric band entirely, never the reverse.
func Classify(v provider.Verdict, thresholds Thresholds) Decision {
	if v.Flagged {
		return Decision{Classification: Hold, ReasonCode: ReasonHoldFlagged}
	}
	switch {
	case v.RiskScore >= thresholds.FlaggedAtOrAbove:
		return Decision{Classification: Hold, ReasonCode: ReasonHoldFlagged}
	case v.RiskScore >= thresholds.PassBelow:
		return Decision{Classification: Hold, ReasonCode: ReasonHoldAmbiguous}
	default:
		return Decision{Classification: Pass, ReasonCode: ReasonPass}
	}
}

// Unavailable is the fail-closed decision for a provider timeout or
// outage: there is no Verdict to classify (the vendor call itself
// failed), so this is not reachable through Classify. Always Hold --
// per "Read this third" in the C3 build spec, a vendor outage never
// silently becomes a pass, and fail-closed is the shipped default.
// There is no backing screening_results row for this decision, hence
// ScreeningResultID stays 0.
func Unavailable() Decision {
	return Decision{Classification: Hold, ReasonCode: ReasonHoldUnavailable}
}

// PassVendorUnavailable is C3.5's fail-open decision: every retry
// against the vendor was exhausted, and the configured outage policy is
// FailOpen. Like Unavailable, this is not reachable through Classify --
// there is no real Verdict to classify -- and has no backing
// screening_results row, so ScreeningResultID stays 0. Never confuse
// this with a real Classify-produced Pass: the whole point of a
// distinct reason code is that an auditor can tell the two apart.
func PassVendorUnavailable() Decision {
	return Decision{Classification: Pass, ReasonCode: ReasonPassVendorUnavailable}
}
