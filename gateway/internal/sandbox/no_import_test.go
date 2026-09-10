package sandbox

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sandboxImportAllowlist is the complete, exhaustive list of files
// outside this package that may import "gateway/internal/sandbox" --
// exactly the two integration points a fully separate code path still
// needs: the HTTP routes that expose it, and main.go wiring it up.
// Nothing else -- in particular, no production handler
// (quotes_handlers.go, orders_handlers.go, status_handlers.go) and no
// C1/C2 client package may ever import this one, per
// c6-api-gateway-build-prompts.md's own "never imported by any
// production code path."
var sandboxImportAllowlist = map[string]bool{
	filepath.Join("internal", "httpapi", "sandbox_handlers.go"): true,
	filepath.Join("internal", "httpapi", "server.go"):           true,
	filepath.Join("cmd", "gatewayd", "main.go"):                 true,
	// C6.9's own ship gate exercises all four sandbox triggers through
	// the real router (its own "Read this third" acceptance criterion)
	// -- test/CI tooling, not a production request-handling path.
	filepath.Join("internal", "replay", "harness.go"): true,
}

// TestSandboxNotImportedOutsideAllowlist parses the import block of
// every .go file in this module (excluding this package's own files
// and _test.go files, which may legitimately reference this package's
// own import path in a comment) and fails if any file outside
// sandboxImportAllowlist imports "gateway/internal/sandbox" -- the
// same mechanical dependency-boundary discipline
// energybroker/internal/provider/dependency_test.go established for
// its own vendor-SDK isolation, adapted to an allowlist since sandbox,
// unlike a vendor SDK, legitimately needs a couple of named
// integration points.
func TestSandboxNotImportedOutsideAllowlist(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	sandboxDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolving internal/sandbox's own absolute path: %v", err)
	}

	const sandboxImportPath = "gateway/internal/sandbox"

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
		if strings.HasPrefix(abs, sandboxDir+string(filepath.Separator)) || abs == sandboxDir {
			return nil // this package's own files
		}

		rel, err := filepath.Rel(moduleRoot, abs)
		if err != nil {
			return err
		}
		if sandboxImportAllowlist[rel] {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == sandboxImportPath {
				t.Errorf("%s imports %q -- not on sandboxImportAllowlist; the sandbox is a fully separate code path and must never be reachable from production handling",
					rel, importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module for sandbox-import check: %v", err)
	}
}

// findModuleRoot walks up from the current package directory until it
// finds go.mod, so this test works regardless of the working directory
// `go test` is invoked from -- same helper every sibling module's own
// dependency test uses.
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
			t.Fatal("could not find go.mod walking up from internal/sandbox")
		}
		dir = parent
	}
}
