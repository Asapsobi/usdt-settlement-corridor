package replay

import (
	"errors"
	"time"
)

// Config controls one replay run against a real, running C1 instance.
// Like C2.10's and C3.9's own Config, this is a seed plus where to find
// that running C1, not a scenario-count mix -- C4's own real-world
// volume is ~100 payouts/day (component-map.md), and this chunk's own
// PERFORMANCE TARGET is explicit that correctness under adversarial
// timing is the point, not throughput.
type Config struct {
	Seed          int64
	LedgerBaseURL string
	LedgerToken   string
}

// DefaultConfig seeds from the current time (so two runs without an
// explicit seed still differ -- there has to be a fresh one to print on
// failure).
func DefaultConfig() Config {
	return Config{Seed: time.Now().UnixNano()}
}

var (
	errMissingLedgerBaseURL = errors.New("replay: Config.LedgerBaseURL is required (a real, running C1 instance)")
	errMissingLedgerToken   = errors.New("replay: Config.LedgerToken is required")
)

func (c Config) validate() error {
	if c.LedgerBaseURL == "" {
		return errMissingLedgerBaseURL
	}
	if c.LedgerToken == "" {
		return errMissingLedgerToken
	}
	return nil
}
