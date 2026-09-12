package auditlog

import (
	"path/filepath"
	"testing"
)

func TestWriteThenTail_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()

	if err := log.Write("sobhan", "halt.set", "ledger", map[string]any{"reason": "test"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Write("sobhan", "halt.clear", "ledger", nil); err != nil {
		t.Fatal(err)
	}

	entries, err := Tail(path, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
	if entries[0].Action != "halt.set" || entries[1].Action != "halt.clear" {
		t.Fatalf("entries = %+v, want halt.set then halt.clear (oldest first)", entries)
	}
}

func TestTail_BoundsToN(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if err := log.Write("sobhan", "action", "target", nil); err != nil {
			t.Fatal(err)
		}
	}
	log.Close()

	entries, err := Tail(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
}

func TestTail_MissingFileReturnsEmpty(t *testing.T) {
	entries, err := Tail(filepath.Join(t.TempDir(), "does-not-exist.jsonl"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("len(entries) = %d, want 0", len(entries))
	}
}
