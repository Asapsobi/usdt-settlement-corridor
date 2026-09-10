// Package customers is C6.1: customer identity, API key issuance, and
// suspension. No deletion, matching this project's own never-hard-delete
// convention from every prior component.
package customers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gateway/internal/db"
)

// Status is one customer's own account status.
type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
)

// Customer is one row of the customers table -- APIKeyHash only, never
// the raw key (see HashAPIKey's own doc comment). WebhookSecret, unlike
// APIKeyHash, is the raw shared secret itself -- C6.6's own delivery
// loop needs it to compute a real HMAC, not just verify one.
type Customer struct {
	ID                 int64
	Name               string
	APIKeyHash         string
	WebhookSecret      string
	WebhookURL         *string
	IsSandbox          bool
	Status             Status
	RateLimitPerMinute *int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ErrNotFound means no customer matches the given id or API key hash.
var ErrNotFound = errors.New("customers: no such customer")

// Store is the customers table's own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

// GenerateAPIKey returns a fresh, random, high-entropy API key (32
// random bytes, hex-encoded, "sk_live_" prefixed so it's visually
// distinct from a sandbox key -- see internal/sandbox's own "sk_test_"
// prefix, C6.7) -- shown to the caller exactly once, by whatever created
// this customer; never logged, never persisted in raw form (only
// HashAPIKey's own output is).
func GenerateAPIKey() (string, error) {
	return generatePrefixedKey("sk_live_")
}

// GenerateSandboxAPIKey is GenerateAPIKey's own sandbox counterpart --
// "sk_test_" prefixed, C6.7's own documented, visually-distinct
// namespace. A sandbox key is issued only by CreateSandbox and is never
// converted into (or accepted in place of) a production one -- see
// invariant 4 in c6-api-gateway-build-prompts.md's own §0.
func GenerateSandboxAPIKey() (string, error) {
	return generatePrefixedKey("sk_test_")
}

func generatePrefixedKey(prefix string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("customers: generating %s API key: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(raw), nil
}

// HashAPIKey hashes a raw API key for storage/lookup. SHA-256, not a
// slow password hash (bcrypt/argon2/scrypt): those exist to slow down
// brute-forcing a LOW-entropy human-chosen secret, which is not this --
// GenerateAPIKey's own 256 bits of randomness is already computationally
// infeasible to brute force, so a fast, deterministic hash (needed for
// an indexed lookup on every single request, unlike a login form) is the
// right tool here, not the wrong one applied cheaply.
func HashAPIKey(rawKey string) string {
	sum := sha256.Sum256([]byte(rawKey))
	return hex.EncodeToString(sum[:])
}

// generateWebhookSecret returns a fresh, random, high-entropy shared
// secret for HMAC-signing this customer's webhook payloads -- same
// entropy and shape as GenerateAPIKey, "whsec_" prefixed (mirrors
// Stripe's own convention, visually distinct from an API key) so it's
// never mistaken for one in a support ticket or a pasted log line.
func generateWebhookSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("customers: generating webhook secret: %w", err)
	}
	return "whsec_" + hex.EncodeToString(raw), nil
}

// Create inserts a new active, production customer and returns both
// the row and the raw API key -- the only moment that raw value ever
// exists outside the caller's own memory. The row's own WebhookSecret
// is returned too (it is stored in plaintext, not hashed -- see
// Customer's own doc comment -- but is still shown to the caller here
// for the same "the caller created this, so the caller already has it"
// reason as the API key).
func (s *Store) Create(ctx context.Context, name string) (Customer, string, error) {
	return s.create(ctx, name, false)
}

// CreateSandbox inserts a new sandbox customer -- C6.7's own fully
// separate identity space (invariant 4: no conversion between a
// sandbox and a production customer, ever). Its API key is
// GenerateSandboxAPIKey's own "sk_test_"-prefixed value, never
// "sk_live_".
func (s *Store) CreateSandbox(ctx context.Context, name string) (Customer, string, error) {
	return s.create(ctx, name, true)
}

func (s *Store) create(ctx context.Context, name string, sandbox bool) (Customer, string, error) {
	genKey := GenerateAPIKey
	if sandbox {
		genKey = GenerateSandboxAPIKey
	}
	rawKey, err := genKey()
	if err != nil {
		return Customer{}, "", err
	}
	hash := HashAPIKey(rawKey)
	webhookSecret, err := generateWebhookSecret()
	if err != nil {
		return Customer{}, "", err
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO customers (name, api_key_hash, webhook_secret, is_sandbox, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, name, api_key_hash, webhook_secret, webhook_url, is_sandbox, status, rate_limit_per_minute, created_at, updated_at
	`, name, hash, webhookSecret, sandbox, string(StatusActive))
	c, err := scanCustomer(row)
	if err != nil {
		return Customer{}, "", fmt.Errorf("customers: creating %q: %w", name, err)
	}
	return c, rawKey, nil
}

// GetByAPIKey looks up the customer whose stored hash matches rawKey --
// hashes rawKey itself (fast, deterministic, see HashAPIKey) and does an
// indexed lookup; never a linear scan comparing rawKey against every
// stored hash.
func (s *Store) GetByAPIKey(ctx context.Context, rawKey string) (Customer, error) {
	hash := HashAPIKey(rawKey)
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, api_key_hash, webhook_secret, webhook_url, is_sandbox, status, rate_limit_per_minute, created_at, updated_at
		FROM customers WHERE api_key_hash = $1
	`, hash)
	c, err := scanCustomer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Customer{}, ErrNotFound
		}
		return Customer{}, fmt.Errorf("customers: looking up by API key: %w", err)
	}
	return c, nil
}

// Get fetches a customer by id.
func (s *Store) Get(ctx context.Context, id int64) (Customer, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, name, api_key_hash, webhook_secret, webhook_url, is_sandbox, status, rate_limit_per_minute, created_at, updated_at
		FROM customers WHERE id = $1
	`, id)
	c, err := scanCustomer(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Customer{}, ErrNotFound
		}
		return Customer{}, fmt.Errorf("customers: fetching %d: %w", id, err)
	}
	return c, nil
}

// Suspend transitions a customer to suspended -- idempotent (suspending
// an already-suspended customer is a no-op success, not an error).
func (s *Store) Suspend(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, StatusSuspended)
}

// Reactivate transitions a customer back to active.
func (s *Store) Reactivate(ctx context.Context, id int64) error {
	return s.setStatus(ctx, id, StatusActive)
}

// WebhookSecretFor returns id's own HMAC signing secret -- satisfies
// webhooks.CustomerLookup directly.
func (s *Store) WebhookSecretFor(ctx context.Context, id int64) (string, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return c.WebhookSecret, nil
}

// WebhookURLFor returns id's configured webhook_url, or "" if none is
// set -- satisfies webhooks.WebhookURLLookup directly, the same
// "the concrete store already has the shape a sibling package's own
// interface needs" pattern c1client.Client satisfies
// webhooks.OrderPoller with.
func (s *Store) WebhookURLFor(ctx context.Context, id int64) (string, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if c.WebhookURL == nil {
		return "", nil
	}
	return *c.WebhookURL, nil
}

// SetWebhookURL configures (or clears, with an empty string) the
// endpoint C6.6's own trigger loop delivers this customer's webhooks
// to. A customer with none set is simply skipped at enqueue time --
// see webhook_deliveries' own migration comment.
func (s *Store) SetWebhookURL(ctx context.Context, id int64, url string) error {
	var arg any
	if url != "" {
		arg = url
	}
	tag, err := s.pool.Exec(ctx, `UPDATE customers SET webhook_url = $1, updated_at = now() WHERE id = $2`, arg, id)
	if err != nil {
		return fmt.Errorf("customers: setting webhook_url for %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) setStatus(ctx context.Context, id int64, status Status) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE customers SET status = $1, updated_at = now() WHERE id = $2
	`, string(status), id)
	if err != nil {
		return fmt.Errorf("customers: setting status of %d to %s: %w", id, status, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanCustomer(row scanRow) (Customer, error) {
	var c Customer
	var status string
	if err := row.Scan(&c.ID, &c.Name, &c.APIKeyHash, &c.WebhookSecret, &c.WebhookURL, &c.IsSandbox, &status, &c.RateLimitPerMinute, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return Customer{}, err
	}
	c.Status = Status(status)
	return c, nil
}
