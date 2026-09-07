package provider

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenVendorImportFragments are substrings that would appear in an
// import PATH for a specific energy vendor's own SDK. Unlike C3's own
// dependency_test.go (which can safely substring-match its vendor names
// anywhere in a file, because C3 never names Chainalysis/TRM/Elliptic
// literally outside a real SDK import), C4 legitimately writes
// "tronsell"/"netts"/"catfee" as plain config strings all over this
// module (§B's own routing-weights config, the Provider name constants
// this very package exports, a later chunk's routing/pricing code) --
// so this test matches only import DECLARATIONS, never arbitrary file
// text, to avoid flagging every legitimate business use of a vendor's
// name as if it were an SDK dependency.
var forbiddenVendorImportFragments = []string{
	"tronsell",
	"netts",
	"catfee",
	"justlend",
}

// TestNoVendorSDKOutsideProviderPackage parses the import block of every
// .go file in this module, excluding internal/provider itself (where a
// real vendor client belongs) and _test.go files (which may legitimately
// name a vendor in a comment or string when documenting this exact
// rule), for an import path containing a forbidden vendor fragment. Same
// posture as C2's and C3's own dependency-boundary tests: a mechanical
// guard against a specific accidental dependency, not a style
// preference.
func TestNoVendorSDKOutsideProviderPackage(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	providerDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/provider's own absolute path: %v", err)
	}

	fset := token.NewFileSet()
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

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			importPath := strings.ToLower(strings.Trim(imp.Path.Value, `"`))
			for _, fragment := range forbiddenVendorImportFragments {
				if strings.Contains(importPath, fragment) {
					t.Errorf("%s imports %q, containing vendor-SDK fragment %q -- vendor integrations belong only in internal/provider, behind EnergyProvider",
						path, importPath, fragment)
				}
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
