package accounts

import (
	"errors"
	"testing"
)

func TestParseCode(t *testing.T) {
	cases := []struct {
		name     string
		code     string
		wantErr  bool
		category string
		segments []string
	}{
		{"slot code", "asset:tron:slot:3", false, "asset", []string{"tron", "slot", "3"}},
		{"two segments", "revenue:fee", false, "revenue", []string{"fee"}},
		{"deep nesting", "expense:loss:reorg", false, "expense", []string{"loss", "reorg"}},
		{"no colon", "revenue", true, "", nil},
		{"empty", "", true, "", nil},
		{"empty segment", "asset::slot", true, "", nil},
		{"trailing colon", "asset:tron:", true, "", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseCode(c.code)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidCode) {
					t.Fatalf("ParseCode(%q): got err %v, want ErrInvalidCode", c.code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCode(%q): unexpected error: %v", c.code, err)
			}
			if got.Category != c.category {
				t.Errorf("ParseCode(%q).Category = %q, want %q", c.code, got.Category, c.category)
			}
			if len(got.Segments) != len(c.segments) {
				t.Fatalf("ParseCode(%q).Segments = %v, want %v", c.code, got.Segments, c.segments)
			}
			for i := range c.segments {
				if got.Segments[i] != c.segments[i] {
					t.Errorf("ParseCode(%q).Segments[%d] = %q, want %q", c.code, i, got.Segments[i], c.segments[i])
				}
			}
		})
	}
}

func TestCodeSegment(t *testing.T) {
	c, err := ParseCode("asset:tron:slot:3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if v, ok := c.Segment(0); !ok || v != "tron" {
		t.Errorf("Segment(0) = (%q, %v), want (\"tron\", true)", v, ok)
	}
	if v, ok := c.Segment(2); !ok || v != "3" {
		t.Errorf("Segment(2) = (%q, %v), want (\"3\", true)", v, ok)
	}
	if _, ok := c.Segment(3); ok {
		t.Errorf("Segment(3) = ok, want out of range")
	}
	if _, ok := c.Segment(-1); ok {
		t.Errorf("Segment(-1) = ok, want out of range")
	}
}
