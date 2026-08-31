package accounts

import (
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidCode = errors.New("accounts: invalid account code")

// Code is a parsed account code: colon-delimited, e.g.
// "asset:tron:slot:3" -> Category "asset", Segments ["tron", "slot", "3"].
// Components dispatch on Category and Segments instead of doing their own
// string surgery on the raw code.
type Code struct {
	Raw      string
	Category string
	Segments []string
}

// ParseCode splits a colon-delimited account code into its category (the
// first segment) and the remaining segments. It rejects the empty string,
// a code with no colon at all, and any empty segment (e.g. "asset::slot").
func ParseCode(code string) (Code, error) {
	if code == "" {
		return Code{}, fmt.Errorf("%w: empty code", ErrInvalidCode)
	}
	parts := strings.Split(code, ":")
	if len(parts) < 2 {
		return Code{}, fmt.Errorf("%w: %q has no colon-delimited segments", ErrInvalidCode, code)
	}
	for _, p := range parts {
		if p == "" {
			return Code{}, fmt.Errorf("%w: %q has an empty segment", ErrInvalidCode, code)
		}
	}
	return Code{Raw: code, Category: parts[0], Segments: parts[1:]}, nil
}

// Segment returns the i'th segment after the category (0-indexed) and
// whether it exists, so callers can do e.g. Segment(0) for "tron" in
// "asset:tron:slot:3" without bounds-checking Segments themselves.
func (c Code) Segment(i int) (string, bool) {
	if i < 0 || i >= len(c.Segments) {
		return "", false
	}
	return c.Segments[i], true
}
