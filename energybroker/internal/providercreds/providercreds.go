// Package providercreds is the DB-backed store for vendor credentials
// (Tronsell/Netts/CatFee) -- see migration 0009's own doc comment and
// docs/03-build/ops-console-build-prompts.md's OC.10: this exists so an
// operator can add or rotate a vendor's own API key through the ops
// console instead of editing env vars and redeploying. cmd/brokerd reads
// this table at startup (after a one-time env-var bootstrap) to build
// its real provider.EnergyProvider instances -- this package holds no
// opinion on how those get constructed, only on storing/retrieving the
// raw credential fields.
package providercreds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"energybroker/internal/db"
)

// Credential is one vendor's stored configuration. APISecret and RealIP
// are optional (Netts has no secret; only Netts uses RealIP).
type Credential struct {
	ProviderName string
	BaseURL      *string
	APIKey       string
	APISecret    *string
	RealIP       *string
	Enabled      bool
	UpdatedAt    time.Time
	UpdatedBy    string
}

// ErrNotFound means no credential row exists for that provider name yet.
var ErrNotFound = errors.New("providercreds: no credentials configured for that provider")

// List returns every configured provider's credentials, whether or not
// enabled -- the console's own "Providers" page (OC.10) renders all of
// them, greying out disabled ones rather than hiding them.
func List(ctx context.Context, q db.Queryer) ([]Credential, error) {
	rows, err := q.Query(ctx, `
		SELECT provider_name, base_url, api_key, api_secret, real_ip, enabled, updated_at, updated_by
		FROM provider_credentials ORDER BY provider_name
	`)
	if err != nil {
		return nil, fmt.Errorf("providercreds: listing: %w", err)
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ProviderName, &c.BaseURL, &c.APIKey, &c.APISecret, &c.RealIP, &c.Enabled, &c.UpdatedAt, &c.UpdatedBy); err != nil {
			return nil, fmt.Errorf("providercreds: scanning row: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providercreds: listing: %w", err)
	}
	return out, nil
}

// Get returns one provider's credentials, or ErrNotFound if none are
// configured yet.
func Get(ctx context.Context, q db.Queryer, providerName string) (Credential, error) {
	var c Credential
	err := q.QueryRow(ctx, `
		SELECT provider_name, base_url, api_key, api_secret, real_ip, enabled, updated_at, updated_by
		FROM provider_credentials WHERE provider_name = $1
	`, providerName).Scan(&c.ProviderName, &c.BaseURL, &c.APIKey, &c.APISecret, &c.RealIP, &c.Enabled, &c.UpdatedAt, &c.UpdatedBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Credential{}, ErrNotFound
		}
		return Credential{}, fmt.Errorf("providercreds: fetching %s: %w", providerName, err)
	}
	return c, nil
}

// Upsert creates or replaces one provider's stored credentials --
// the console's own "add/rotate" action (OC.10). Never partial: a
// rotation always supplies every field that provider needs, matching
// this project's existing "no silent partial state" convention for
// operator-facing writes.
func Upsert(ctx context.Context, q db.Queryer, c Credential) error {
	_, err := q.Exec(ctx, `
		INSERT INTO provider_credentials (provider_name, base_url, api_key, api_secret, real_ip, enabled, updated_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, now(), $7)
		ON CONFLICT (provider_name) DO UPDATE SET
			base_url = EXCLUDED.base_url, api_key = EXCLUDED.api_key, api_secret = EXCLUDED.api_secret,
			real_ip = EXCLUDED.real_ip, enabled = EXCLUDED.enabled, updated_at = now(), updated_by = EXCLUDED.updated_by
	`, c.ProviderName, c.BaseURL, c.APIKey, c.APISecret, c.RealIP, c.Enabled, c.UpdatedBy)
	if err != nil {
		return fmt.Errorf("providercreds: upserting %s: %w", c.ProviderName, err)
	}
	return nil
}

// BootstrapFromEnv inserts one row per provider named in fromEnv, but
// ONLY if that provider has no row yet -- a one-time migration path for
// a deployment that already had real credentials in env vars before this
// table existed. Never overwrites a row an operator has since edited
// through the console, even if the env var still carries an old value.
func BootstrapFromEnv(ctx context.Context, q db.Queryer, fromEnv []Credential) error {
	for _, c := range fromEnv {
		if c.APIKey == "" {
			continue // nothing in the env for this provider -- nothing to bootstrap
		}
		_, err := q.Exec(ctx, `
			INSERT INTO provider_credentials (provider_name, base_url, api_key, api_secret, real_ip, enabled, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, $5, true, now(), 'env-bootstrap')
			ON CONFLICT (provider_name) DO NOTHING
		`, c.ProviderName, c.BaseURL, c.APIKey, c.APISecret, c.RealIP)
		if err != nil {
			return fmt.Errorf("providercreds: bootstrapping %s from env: %w", c.ProviderName, err)
		}
	}
	return nil
}
