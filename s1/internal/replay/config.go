package replay

import "time"

// Config controls one replay run. Unlike every prior component's own
// replay Config, S1 has no external service dependency to point at (no
// real C1, no real vendor) -- everything it needs is its own database
// plus an in-process FakeKMSClient, so Config is just a seed.
type Config struct {
	Seed int64
}

// DefaultConfig seeds from the current time, so two runs without an
// explicit seed still differ.
func DefaultConfig() Config {
	return Config{Seed: time.Now().UnixNano()}
}
