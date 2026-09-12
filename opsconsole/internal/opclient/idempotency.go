package opclient

import (
	"crypto/rand"
	"encoding/hex"
)

// idempotencyKey returns a fresh random key for one write call. The
// console never retries a write on a caller's behalf -- each button click
// is exactly one attempt -- so a random key per call, not a stable one
// derived from the request body, is the right choice here (contrast
// dispatcher's own orchestrate loop, which derives a STABLE key because it
// retries the same logical operation across ticks; the console has no
// equivalent retry loop to be idempotent against).
func idempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "opsconsole:" + hex.EncodeToString(b)
}
