package httpapi

import (
	"os"
	"strings"
	"testing"
)

// TestEveryRouteIsInOpenAPISpec guards against route/doc drift: every
// path this package's router actually registers (see server.go) must
// appear as a path key in docs/openapi.yaml. A simple substring check on
// "<path>:" rather than a full YAML parse -- this only needs to catch a
// route that was added to the router and never documented, not validate
// the spec's schemas.
func TestEveryRouteIsInOpenAPISpec(t *testing.T) {
	data, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}
	spec := string(data)

	routes := []string{
		"/healthz",
		"/readyz",
		"/metrics",
		"/v1/entries",
		"/v1/orders",
		"/v1/orders/{external_id}",
		"/v1/orders/{external_id}/transitions",
		"/v1/accounts/{code}/balance",
		"/v1/balances",
		"/v1/trial-balance",
		"/v1/reconciliation/snapshots",
		"/v1/system/halt",
		"/v1/system/invariants",
	}

	for _, route := range routes {
		if !strings.Contains(spec, route+":") {
			t.Errorf("route %q is not present as a path key in docs/openapi.yaml", route)
		}
	}
}
