// Package session signs and verifies the ops console's own cookie --
// no database, no server-side session table. A cookie carries the
// operator's identity and, optionally, their own S1 approver token
// (invariant 1: that token is never held anywhere else, see OC.1/OC.7 in
// docs/03-build/ops-console-build-prompts.md).
package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Session is what one signed cookie carries.
type Session struct {
	Username        string    `json:"username"`
	DisplayName     string    `json:"display_name"`
	S1ApproverToken string    `json:"s1_approver_token,omitempty"`
	IssuedAt        time.Time `json:"issued_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// ErrInvalidSignature means the cookie's signature didn't match --
// tampered, or signed with a different secret.
var ErrInvalidSignature = errors.New("session: invalid signature")

// ErrExpired means the cookie's own signature is valid but ExpiresAt has
// passed.
var ErrExpired = errors.New("session: expired")

// Signer signs and verifies Session values against one server-side
// secret.
type Signer struct {
	secret []byte
}

// NewSigner returns a Signer using secret (OC_SESSION_SECRET) to
// HMAC-sign every cookie this process issues. Fails loud, at
// construction, on a secret too short to matter -- the same "a weak
// value here would actually matter" posture OC.0's own doc comment
// gives this specific config value.
func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("session: OC_SESSION_SECRET must be at least 32 bytes, got %d", len(secret))
	}
	return &Signer{secret: []byte(secret)}, nil
}

// Sign returns a cookie value encoding s, HMAC-SHA256-signed against the
// Signer's own secret.
func (sg *Signer) Sign(s Session) (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("session: encoding: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, sg.secret)
	mac.Write([]byte(payloadB64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payloadB64 + "." + sig, nil
}

// Verify parses and validates a cookie value produced by Sign,
// rejecting a tampered payload, a signature made with a different
// secret, or an expired one.
func (sg *Signer) Verify(cookieValue string) (Session, error) {
	dot := -1
	for i := len(cookieValue) - 1; i >= 0; i-- {
		if cookieValue[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return Session{}, ErrInvalidSignature
	}
	payloadB64, sig := cookieValue[:dot], cookieValue[dot+1:]

	mac := hmac.New(sha256.New, sg.secret)
	mac.Write([]byte(payloadB64))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return Session{}, ErrInvalidSignature
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return Session{}, ErrInvalidSignature
	}
	var s Session
	if err := json.Unmarshal(payload, &s); err != nil {
		return Session{}, ErrInvalidSignature
	}
	if time.Now().After(s.ExpiresAt) {
		return Session{}, ErrExpired
	}
	return s, nil
}
