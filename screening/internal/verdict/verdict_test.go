package verdict

import (
	"testing"

	"screening/internal/provider"
)

var testThresholds = Thresholds{PassBelow: 0.5, FlaggedAtOrAbove: 0.85}

func TestClassify_ThresholdBoundaries(t *testing.T) {
	tests := []struct {
		name           string
		riskScore      float64
		wantClass      Classification
		wantReasonCode string
	}{
		{"well below pass threshold", 0.0, Pass, ReasonPass},
		{"just below pass threshold", 0.499999, Pass, ReasonPass},
		{"exactly at pass threshold falls into hold", 0.5, Hold, ReasonHoldAmbiguous},
		{"mid ambiguous band", 0.7, Hold, ReasonHoldAmbiguous},
		{"just below flagged threshold", 0.849999, Hold, ReasonHoldAmbiguous},
		{"exactly at flagged threshold falls into flagged", 0.85, Hold, ReasonHoldFlagged},
		{"well above flagged threshold", 1.0, Hold, ReasonHoldFlagged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := provider.Verdict{RiskScore: tt.riskScore, Flagged: false}
			got := Classify(v, testThresholds)
			if got.Classification != tt.wantClass {
				t.Errorf("Classify(score=%v).Classification = %v, want %v", tt.riskScore, got.Classification, tt.wantClass)
			}
			if got.ReasonCode != tt.wantReasonCode {
				t.Errorf("Classify(score=%v).ReasonCode = %q, want %q", tt.riskScore, got.ReasonCode, tt.wantReasonCode)
			}
		})
	}
}

func TestClassify_DeterministicRepeat(t *testing.T) {
	v := provider.Verdict{RiskScore: 0.7, Flagged: false}
	first := Classify(v, testThresholds)
	for i := 0; i < 100; i++ {
		got := Classify(v, testThresholds)
		if got != first {
			t.Fatalf("Classify is not pure: call %d returned %+v, first call returned %+v", i, got, first)
		}
	}
}

func TestClassify_FlaggedOverridesScoreRegardlessOfValue(t *testing.T) {
	scores := []float64{0.0, 0.1, 0.49, 0.5, 0.849, 0.85, 1.0}
	for _, score := range scores {
		v := provider.Verdict{RiskScore: score, Flagged: true}
		got := Classify(v, testThresholds)
		if got.Classification != Hold {
			t.Errorf("Classify(score=%v, Flagged=true).Classification = %v, want Hold", score, got.Classification)
		}
		if got.ReasonCode != ReasonHoldFlagged {
			t.Errorf("Classify(score=%v, Flagged=true).ReasonCode = %q, want %q", score, got.ReasonCode, ReasonHoldFlagged)
		}
	}
}

// TestClassify_NeverProducesReject documents, structurally, that Classify
// cannot return Reject: the Classification type defines exactly two
// constants (Pass, Hold) and no third. This test is a belt-and-suspenders
// check across a spread of inputs, since Go's type system doesn't
// actually forbid an out-of-range int being assigned to Classification --
// only this package's own code discipline (never constructing one) does.
func TestClassify_NeverProducesReject(t *testing.T) {
	scores := []float64{-1, 0, 0.25, 0.5, 0.5000001, 0.7, 0.849999, 0.85, 0.999999, 1, 2}
	flaggedOptions := []bool{true, false}
	thresholdOptions := []Thresholds{
		testThresholds,
		DefaultThresholds,
		{PassBelow: 0, FlaggedAtOrAbove: 0},
		{PassBelow: 1, FlaggedAtOrAbove: 1},
	}

	for _, thresholds := range thresholdOptions {
		for _, flagged := range flaggedOptions {
			for _, score := range scores {
				got := Classify(provider.Verdict{RiskScore: score, Flagged: flagged}, thresholds)
				if got.Classification != Pass && got.Classification != Hold {
					t.Fatalf("Classify produced an unrecognized Classification %v (not Pass or Hold) for score=%v flagged=%v thresholds=%+v",
						got.Classification, score, flagged, thresholds)
				}
			}
		}
	}
}

func TestUnavailable_IsFailClosedHold(t *testing.T) {
	d := Unavailable()
	if d.Classification != Hold {
		t.Fatalf("Unavailable().Classification = %v, want Hold", d.Classification)
	}
	if d.ReasonCode != ReasonHoldUnavailable {
		t.Fatalf("Unavailable().ReasonCode = %q, want %q", d.ReasonCode, ReasonHoldUnavailable)
	}
	if d.ScreeningResultID != 0 {
		t.Fatalf("Unavailable().ScreeningResultID = %d, want 0 (no backing result row)", d.ScreeningResultID)
	}
}

func TestClassify_ScreeningResultIDIsNeverSetByThisPackage(t *testing.T) {
	// Classify has no cache access -- it must never fabricate a FK. The
	// caller (C3.4) is responsible for filling this in once the Verdict
	// is actually persisted.
	v := provider.Verdict{RiskScore: 0.1, Flagged: false}
	got := Classify(v, testThresholds)
	if got.ScreeningResultID != 0 {
		t.Fatalf("Classify set ScreeningResultID = %d, want 0 (Classify is pure and has no cache access)", got.ScreeningResultID)
	}
}
