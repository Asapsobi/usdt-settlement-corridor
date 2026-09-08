package httpapi

import (
	"os"
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
)

// openAPISpec is the subset of docs/openapi.yaml these tests assert on.
type openAPISpec struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openAPISpec {
	t.Helper()
	data, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}
	var spec openAPISpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("docs/openapi.yaml is not valid YAML: %v", err)
	}
	return spec
}

func TestEveryRouteIsInOpenAPISpec(t *testing.T) {
	spec := loadSpec(t)

	routes := map[string][]string{
		"/healthz":                {"get"},
		"/readyz":                 {"get"},
		"/metrics":                {"get"},
		"/v1/dispatch":            {"post"},
		"/v1/dispatch/{order_id}": {"get"},
		"/v1/slots":               {"get"},
		"/v1/slots/{id}/retire":   {"post"},
		"/v1/system/invariants":   {"get"},
	}

	for route, methods := range routes {
		operations, ok := spec.Paths[route]
		if !ok {
			t.Errorf("route %q is not present as a path key in docs/openapi.yaml", route)
			continue
		}
		for _, method := range methods {
			if _, ok := operations[method]; !ok {
				t.Errorf("route %q is documented, but its %s operation is not", route, method)
			}
		}
	}
}

// TestOpenAPIRefsResolve catches a $ref naming a component that doesn't
// exist -- a dangling reference breaks a code generator just as
// completely as a syntax error, and is just as invisible to a reader.
func TestOpenAPIRefsResolve(t *testing.T) {
	spec := loadSpec(t)
	data, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}

	refPattern := regexp.MustCompile(`\$ref:\s*"#/components/schemas/(\w+)"`)
	for _, match := range refPattern.FindAllStringSubmatch(string(data), -1) {
		name := match[1]
		if _, ok := spec.Components.Schemas[name]; !ok {
			t.Errorf("dangling $ref to schema %q", name)
		}
	}
}
