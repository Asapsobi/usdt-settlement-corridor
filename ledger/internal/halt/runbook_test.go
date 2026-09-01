package halt

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestRunbookCoversEveryReason is C1.10's acceptance criterion: every
// halt reason defined in code has a section in the runbook. Deliberately
// a plain unit test (no //go:build integration, no database) so it runs
// on every `go test ./...` -- documentation drift is a build failure
// the moment a new Reason constant is added above without a matching
// docs/runbook.md section, not something an operator discovers at 3am.
func TestRunbookCoversEveryReason(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	runbookPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "runbook.md")

	content, err := os.ReadFile(runbookPath)
	if err != nil {
		t.Fatalf("reading %s: %v", runbookPath, err)
	}
	text := string(content)

	for _, reason := range KnownReasons {
		if !strings.Contains(text, reason) {
			t.Errorf("docs/runbook.md has no section for halt reason %q (see internal/halt.KnownReasons)", reason)
		}
	}
}
