package kmssign

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenIdentifiers are qualified references to actual signing- or
// key-material-capable constructs -- deliberately NOT the bare word
// "PrivateKey" or "Sign", because this module's own vocabulary for
// REJECTING or describing key material (in comments, in
// SigningService's own doc comments, in error messages) legitimately
// contains those words throughout; a naive substring match would flag
// the very code that enforces the guarantee. Mirrors
// depositwatcher/internal/addresses/no_signing_test.go's own reasoning,
// adapted to this package's own dependency (secp256k1's ecdsa
// sub-package and private-key constructors) rather than bip32/bip39.
var forbiddenIdentifiers = []string{
	"secp256k1.PrivKeyFromBytes",
	"secp256k1.GeneratePrivateKey",
	"ecdsa.Sign(",
	"aws-sdk-go-v2/service/kms", // the real KMS SDK import path, once added -- belongs only here
}

// forbiddenImportFragments are import-path substrings checked against
// every import declaration outside this package -- narrower and cheaper
// than the full-text scan above, but import-only, matching the pattern
// energybroker's/screening's own dependency tests use for vendor-SDK
// boundaries.
var forbiddenImportFragments = []string{
	"aws-sdk-go-v2/service/kms",
}

// TestNoSigningMaterialOutsideKMSSignPackage scans every .go file in this
// module, excluding internal/kmssign itself (where signing-capable code
// belongs) and _test.go files (which may legitimately name these
// identifiers in comments or error-message assertions), for a forbidden
// identifier or import. Same mechanical-guard posture as every prior
// component's own dependency-boundary test.
func TestNoSigningMaterialOutsideKMSSignPackage(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	kmssignDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/kmssign's own absolute path: %v", err)
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
		if strings.HasPrefix(abs, kmssignDir+string(filepath.Separator)) || abs == kmssignDir {
			return nil // internal/kmssign is where signing-capable code belongs
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		content := string(data)
		for _, forbidden := range forbiddenIdentifiers {
			if strings.Contains(content, forbidden) {
				t.Errorf("%s references forbidden identifier %q -- signing-capable code belongs only in internal/kmssign, behind KMSClient", path, forbidden)
			}
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			importPath := strings.ToLower(strings.Trim(imp.Path.Value, `"`))
			for _, fragment := range forbiddenImportFragments {
				if strings.Contains(importPath, fragment) {
					t.Errorf("%s imports %q, containing forbidden fragment %q -- the KMS SDK belongs only in internal/kmssign", path, importPath, fragment)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module for signing-material check: %v", err)
	}
}

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
			t.Fatal("could not find go.mod walking up from internal/kmssign")
		}
		dir = parent
	}
}
