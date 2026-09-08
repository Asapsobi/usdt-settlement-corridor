//go:build integration

// Requires a real, reachable Postgres 16 instance; run via
// `make test-integration`.
package slots_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"s1/internal/db"
	"s1/internal/kmssign"
	"s1/internal/slots"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dbURL := os.Getenv("S1_TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("S1_TEST_DATABASE_URL not set; skipping integration test")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	migrationsDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	sqlDB, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("opening for migration: %v", err)
	}
	defer sqlDB.Close()
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		t.Fatalf("running migrations: %v", err)
	}

	ctx := context.Background()
	pool, err := db.Open(ctx, db.Config{DatabaseURL: dbURL})
	if err != nil {
		t.Fatalf("opening pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE s1_slot_keys RESTART IDENTITY`); err != nil {
		t.Fatalf("truncating tables: %v", err)
	}
	return pool
}

// pubKeyGetterAdapter adapts *kmssign.Wrapper (over a FakeKMSClient) to
// slots.PublicKeyGetter -- a thin passthrough, since Wrapper's own
// GetPublicKey already matches the interface exactly; kept as its own
// named type only so the test below reads as "against a real Wrapper,"
// not "against a hand-rolled fake of the interface."
type pubKeyGetterAdapter struct{ w *kmssign.Wrapper }

func (p pubKeyGetterAdapter) GetPublicKey(ctx context.Context, keyID string) ([33]byte, error) {
	return p.w.GetPublicKey(ctx, keyID)
}

func TestRegister_DerivesAndStoresARealTronAddress(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fake)
	store := slots.NewStore(pool, pubKeyGetterAdapter{wrapper})

	key, err := store.Register(ctx, 1, "kms-key-slot-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if key.TronAddress == "" {
		t.Fatal("Register produced an empty TronAddress")
	}
	if key.TronAddress[0] != 'T' {
		t.Fatalf("TronAddress = %q, want it to start with 'T' (base58check of version byte 0x41)", key.TronAddress)
	}
	if key.Status != slots.StatusActive {
		t.Fatalf("Status = %s, want ACTIVE", key.Status)
	}

	got, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TronAddress != key.TronAddress {
		t.Fatalf("Get returned address %q, want %q (same as Register produced)", got.TronAddress, key.TronAddress)
	}
}

func TestRegister_RejectsDuplicateSlotID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fake)
	store := slots.NewStore(pool, pubKeyGetterAdapter{wrapper})

	if _, err := store.Register(ctx, 1, "kms-key-a"); err != nil {
		t.Fatalf("Register (1st): %v", err)
	}
	if _, err := store.Register(ctx, 1, "kms-key-b"); !errors.Is(err, slots.ErrDuplicateSlot) {
		t.Fatalf("Register (duplicate slot id) error = %v, want ErrDuplicateSlot", err)
	}
}

func TestRegister_RejectsDuplicateKMSKeyID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fake)
	store := slots.NewStore(pool, pubKeyGetterAdapter{wrapper})

	if _, err := store.Register(ctx, 1, "shared-key"); err != nil {
		t.Fatalf("Register (1st): %v", err)
	}
	if _, err := store.Register(ctx, 2, "shared-key"); !errors.Is(err, slots.ErrDuplicateSlot) {
		t.Fatalf("Register (duplicate kms_key_id) error = %v, want ErrDuplicateSlot", err)
	}
}

func TestRetire_OneWayTransition(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	fake := kmssign.NewFakeKMSClient(1)
	wrapper := kmssign.NewWrapper(fake)
	store := slots.NewStore(pool, pubKeyGetterAdapter{wrapper})

	if _, err := store.Register(ctx, 1, "kms-key-1"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := store.Retire(ctx, 1); err != nil {
		t.Fatalf("Retire: %v", err)
	}

	got, err := store.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != slots.StatusRetired {
		t.Fatalf("Status = %s, want RETIRED", got.Status)
	}
	if got.RetiredAt == nil {
		t.Fatal("RetiredAt is nil on a RETIRED slot")
	}

	if err := store.Retire(ctx, 1); !errors.Is(err, slots.ErrAlreadyRetired) {
		t.Fatalf("Retire (already retired) error = %v, want ErrAlreadyRetired", err)
	}

	// Prove the one-way transition is enforced at the DB level too, not
	// just by Store's own Go-level check above -- a raw UPDATE attempting
	// to un-retire must be rejected by the trigger itself.
	_, err = pool.Exec(ctx, `UPDATE s1_slot_keys SET status = 'ACTIVE' WHERE slot_id = 1`)
	if err == nil {
		t.Fatal("a raw UPDATE un-retiring a slot succeeded; want the DB trigger to reject it")
	}
}

func TestGet_NotFound(t *testing.T) {
	pool := testPool(t)
	store := slots.NewStore(pool, pubKeyGetterAdapter{kmssign.NewWrapper(kmssign.NewFakeKMSClient(1))})

	if _, err := store.Get(context.Background(), 99); !errors.Is(err, slots.ErrSlotNotFound) {
		t.Fatalf("Get error = %v, want ErrSlotNotFound", err)
	}
}
