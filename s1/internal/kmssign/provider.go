// Package kmssign is the only package in this module that may hold,
// derive, or produce anything signing-capable. Every other package
// reaches signing only through the SigningService interface
// (internal/requests) and, beneath that, only through KMSClient below --
// never through a private key of its own. See dependency_test.go, which
// enforces that mechanically, and
// docs/02-architecture/s1-key-custody-architecture.md's own "Signing
// mechanics: TRON over KMS" section, which this package implements.
package kmssign

import (
	"context"
	"errors"
)

// KMSClient is the thin interface over a real cloud KMS's own asymmetric
// signing API this package needs -- defined here (the consumer), not
// depended on directly everywhere, matching this project's established
// convention (e.g. energybroker's PriceSource, reservations.BufferReserver).
// A real implementation (calling AWS KMS or GCP Cloud KMS) is a separate,
// later adapter behind this same interface; nothing in this package or
// its own tests requires one to exist.
type KMSClient interface {
	// GetPublicKey returns keyID's own public key, DER-encoded
	// (SubjectPublicKeyInfo) -- a read of public material only, never
	// touches the private side.
	GetPublicKey(ctx context.Context, keyID string) (derPublicKey []byte, err error)

	// Sign returns a DER-encoded ECDSA (r, s) signature over digest,
	// produced entirely inside the KMS/HSM boundary -- the private key
	// underlying keyID never leaves it, and this call's own return value
	// never contains anything from which it could be reconstructed.
	Sign(ctx context.Context, keyID string, digest [32]byte) (derSignature []byte, err error)
}

// ErrSignatureDoesNotMatchKey is Wrapper.Sign's own result when neither
// candidate recovery id's recovered public key matches the slot's known
// public key -- a real, serious condition (a KMS response that doesn't
// correspond to what was asked), not a parsing failure.
var ErrSignatureDoesNotMatchKey = errors.New("kmssign: recovered public key does not match the expected slot key")

// ErrMalformedSignature is returned when KMS's own response cannot be
// parsed as a DER ECDSA signature at all.
var ErrMalformedSignature = errors.New("kmssign: malformed DER signature from KMS")

// ErrMalformedPublicKey is returned when KMS's own GetPublicKey response
// cannot be parsed as a DER-encoded secp256k1 public key.
var ErrMalformedPublicKey = errors.New("kmssign: malformed DER public key from KMS")
