// Package replay is C6.9: the ship gate. Drives gateway's own real
// httpapi.Server (in-process, over its real router -- not a second
// HTTP hop) against a REAL running C1 (ledger) and C2 (depositwatcher),
// through the exact SCENARIO MIX c6-api-gateway-build-prompts.md's own
// C6.9 names, then checks every FINAL ASSERTION it lists. Exit code 0
// (Report.Passed()) is the artifact a production sign-off points at.
//
// C3, C4, C5, and S1 are never started: gateway itself never calls any
// of them (WHAT C6 IS NOT, §0), and an order's own progress through
// screened/dispatching/settled is produced here by this package's own
// ledgerFixture posting the exact same C1 transition calls those real
// components would -- harness-only, never a capability gateway itself
// legitimately needs, exactly the same posture screening's,
// dispatcher's, and energybroker's own replay harnesses already take
// toward whichever upstream isn't part of their own local test.
package replay

import "time"

// Config configures one Run.
type Config struct {
	// Seed drives every random choice this run makes (external_id
	// suffixes, scenario ordering where it matters) -- same seed
	// replays byte-identical, per every sibling C*.9's own discipline.
	Seed int64

	LedgerBaseURL, LedgerToken   string
	WatcherBaseURL, WatcherToken string
}

// DefaultConfig returns a Config with a time-based Seed -- override it
// explicitly to reproduce a specific failure.
func DefaultConfig() Config {
	return Config{Seed: time.Now().UnixNano()}
}
