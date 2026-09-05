package replay

import "time"

// Config controls one replay run against a real, running C1 instance.
// Unlike C1.9's Config (which scales to thousands of generated orders),
// C2's own real-world volume is ~100 deposits/day -- this chunk's own
// PERFORMANCE TARGET is explicit that correctness under adversarial
// conditions is the point, not throughput -- so Config is a seed plus
// where to find that running C1, not a scenario-count mix.
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

func (c Config) validate() error {
	if c.LedgerBaseURL == "" {
		return errMissingLedgerBaseURL
	}
	if c.LedgerToken == "" {
		return errMissingLedgerToken
	}
	return nil
}
