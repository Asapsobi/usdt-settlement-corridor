// Package addresses derives watch-only EVM (BSC) deposit addresses from an
// extended PUBLIC key. It never holds, derives, or accepts a private key or
// seed phrase -- see the guards in derive.go and the dependency-scan test in
// no_signing_test.go, both of which exist specifically to make that a
// structural property of this package, not a promise about how it happens
// to be used today.
package addresses

import (
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/sha3"
)

// Address is an EIP-55 checksum-encoded EVM address, e.g. "0xAb58...".
// Always produced by toChecksumAddress; never assembled by hand elsewhere
// in this package, so there is exactly one place the checksum can be
// gotten wrong.
type Address string

// keccak256 is the ONLY cryptographic primitive this package uses beyond
// elliptic-curve point arithmetic (isolated in derive.go). sha3.LegacyKeccak256
// is a pure hash function with no signing capability of any kind -- unlike
// the secp256k1 library derive.go has to depend on for point decompression,
// there is no tension here to document.
func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// toChecksumAddress renders a 20-byte address per EIP-55: each hex letter
// (a-f) in the lowercase address is uppercased if the corresponding nibble
// of keccak256(lowercase hex string) is >= 8, else left lowercase. Digits
// are never touched. This is what lets a typo'd address be caught by a
// mixed-case check instead of silently going to the wrong account.
func toChecksumAddress(addr20 []byte) Address {
	lower := hex.EncodeToString(addr20)
	hash := hex.EncodeToString(keccak256([]byte(lower)))

	out := make([]byte, len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if c >= '0' && c <= '9' {
			out[i] = c
			continue
		}
		if hexNibble(hash[i]) >= 8 {
			out[i] = c - 'a' + 'A'
		} else {
			out[i] = c
		}
	}
	return Address("0x" + string(out))
}

func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return -1 // unreachable for a hex.EncodeToString output
	}
}

// ValidChecksum reports whether addr is both well-formed (0x + 40 hex
// chars) and correctly EIP-55 checksummed. An all-lowercase or all-
// uppercase address is well-formed but is NOT considered checksummed --
// EIP-55 addresses always carry mixed case as the whole point of the
// scheme, so treating an unchecksummed address as trivially "valid" would
// defeat the reason this encoding exists.
func ValidChecksum(addr Address) bool {
	s := string(addr)
	if !strings.HasPrefix(s, "0x") || len(s) != 42 {
		return false
	}
	raw, err := hex.DecodeString(strings.ToLower(s[2:]))
	if err != nil {
		return false
	}
	return toChecksumAddress(raw) == addr
}

// mustChecksumAddress panics on a length mismatch, which would be a bug in
// this package's own caller (derive.go always passes exactly 20 bytes),
// never a reachable error from untrusted input.
func mustChecksumAddress(addr20 []byte) Address {
	if len(addr20) != 20 {
		panic(fmt.Sprintf("addresses: internal error: expected 20 bytes, got %d", len(addr20)))
	}
	return toChecksumAddress(addr20)
}
