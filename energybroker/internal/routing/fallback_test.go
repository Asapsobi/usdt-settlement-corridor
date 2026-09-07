package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunbookCoversEveryFallbackReason is C4.6's own documentation-drift
// guard: every fallback reason defined in code has a section in
// docs/runbook-energy-fallback.md. Deliberately a plain unit test (no
// //go:build integration, no database) so it runs on every
// `go test ./...` -- mirroring C1.10's own TestRunbookCoversEveryReason
// in ledger/internal/halt exactly.
func TestRunbookCoversEveryFallbackReason(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	runbookPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "runbook-energy-fallback.md")

	content, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("reading %s: %v", runbookPath, err)
	}
	text := string(content)

	for _, reason := range KnownFallbackReasons {
		if !strings.Contains(text, reason) {
			t.Errorf("docs/runbook-energy-fallback.md has no section for fallback reason %q (see routing.KnownFallbackReasons)", reason)
		}
	}
}

func TestReasonForSelection(t *testing.T) {
	tests := []struct {
		selectionReason string
		want            string
		wantErr         bool
	}{
		{ReasonFallbackLadder, FallbackReasonAllUnhealthy, false},
		{ReasonManualRequired, FallbackReasonAllOverCeiling, false},
		{ReasonWeighted, "", true},
		{"garbage", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.selectionReason, func(t *testing.T) {
			got, err := reasonForSelection(tc.selectionReason)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("reasonForSelection(%q): expected an error, got %q", tc.selectionReason, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("reasonForSelection(%q): unexpected error: %v", tc.selectionReason, err)
			}
			if got != tc.want {
				t.Fatalf("reasonForSelection(%q) = %q, want %q", tc.selectionReason, got, tc.want)
			}
		})
	}
}

func TestResolve_RejectsEmptyResolution(t *testing.T) {
	r := NewRouter(nil, nil, 1)
	err := r.Resolve(context.Background(), 1, "", "ops-oncall") // fails validation before ever touching r.pool
	if !errors.Is(err, ErrEmptyResolution) {
		t.Fatalf("Resolve error = %v, want ErrEmptyResolution", err)
	}
}

func TestResolve_RejectsEmptyActor(t *testing.T) {
	r := NewRouter(nil, nil, 1)
	err := r.Resolve(context.Background(), 1, "vendors recovered", "") // fails validation before ever touching r.pool
	if !errors.Is(err, ErrEmptyActor) {
		t.Fatalf("Resolve error = %v, want ErrEmptyActor", err)
	}
}
