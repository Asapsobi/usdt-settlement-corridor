package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenVendorImportFragments are substrings that would appear in an
// import path for a specific screening vendor's own SDK -- Chainalysis,
// TRM Labs, and Elliptic are the three candidates component-map.md names
// without picking one (see §B of the C3 build spec: "vendor abstraction,
// not vendor choice"). None of these is an actual dependency yet; this
// test exists so that the day one is added, it can only ever land inside
// internal/provider, never leak into internal/verdict, internal/holds, or
// anywhere else that should only ever see the ScreeningProvider interface.
var forbiddenVendorImportFragments = []string{
	"chainalysis",
	"trmlabs",
	"trm-labs",
	"elliptic-co",
	"goelliptic",
}

// TestNoVendorSDKOutsideProviderPackage scans every .go file in this
// module, excluding internal/provider itself (and _test.go files, which
// may legitimately name a vendor in a comment or string when documenting
// this exact rule), for a forbidden vendor import fragment. Same posture
// as depositwatcher's internal/addresses/no_signing_test.go: a mechanical
// guard against a specific accidental dependency, not a style preference.
func TestNoVendorSDKOutsideProviderPackage(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	providerDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/provider's own absolute path: %v", err)
	}

	err = filepath.Walk(moduleRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(abs, providerDir+string(filepath.Separator)) || abs == providerDir {
			return nil // internal/provider is where a vendor SDK belongs
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		lower := strings.ToLower(string(data))
		for _, fragment := range forbiddenVendorImportFragments {
			if strings.Contains(lower, fragment) {
				t.Errorf("%s references vendor-SDK fragment %q -- vendor integrations belong only in internal/provider, behind ScreeningProvider", path, fragment)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module for vendor-SDK check: %v", err)
	}
}

// findModuleRoot walks up from the current package directory until it
// finds go.mod, so this test works regardless of the working directory
// `go test` is invoked from.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from internal/provider")
		}
		dir = parent
	}
}
