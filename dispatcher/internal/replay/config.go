package replay

import "time"

// Config controls one replay run. Like C4's own replay Config, C5 has a
// real external dependency to point at (a real, already-running
// ledgerd) -- unlike C4, nothing else here is real: no S1 exists (see
// "Read this first" in the build spec), and this harness does not
// assume a reachable TRON node any more than C4's own did. Every other
// external boundary (S1's SigningService, C4's own reservation client,
// the chain itself) is an in-process fake, scenario-local, so no run
// needs a second real service.
type Config struct {
	Seed          int64
	LedgerBaseURL string
	LedgerToken   string
}

// DefaultConfig seeds from the current time, so two runs without an
// explicit seed still differ.
func DefaultConfig() Config {
	return Config{Seed: time.Now().UnixNano()}
}
