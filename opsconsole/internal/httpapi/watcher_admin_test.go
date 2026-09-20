package httpapi

import (
	"strings"
	"testing"
)

// TestWatcherRetireTemplate_ConfirmCopyMatchesRealEffect is OC.17's own
// acceptance criterion: the retire confirmation copy must name the
// actual effect of C2's real route, not an invented one. Language
// checked against depositwatcher/internal/addresses/store.go's own doc
// comment on Retire: "explicit and permanent... a retired address can
// never be reassigned or re-watched" (invariant 6).
func TestWatcherRetireTemplate_ConfirmCopyMatchesRealEffect(t *testing.T) {
	for _, phrase := range []string{"permanent", "can never be reassigned or re-watched"} {
		if !strings.Contains(watcherRetireContent, phrase) {
			t.Errorf("watcher_retire template is missing real-effect language %q", phrase)
		}
	}
}
