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
		Schemas    map[string]any `yaml:"schemas"`
		Parameters map[string]any `yaml:"parameters"`
		Responses  map[string]any `yaml:"responses"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openAPISpec {
	t.Helper()
	data, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatalf("reading docs/openapi.yaml: %v", err)
	}
	var spec openAPISpec
	// Parsing rather than substring-matching, same reasoning as C1's and
	// C2's own equivalent test: the spec is a published contract, quite
	// possibly fed to a code generator one day, and a substring check
	// passes happily on a file no YAML parser would accept.
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("docs/openapi.yaml is not valid YAML: %v", err)
	}
	return spec
}

// TestEveryRouteIsInOpenAPISpec guards against route/doc drift: every
// path this package's router actually registers (see server.go) must
// appear as a path key in docs/openapi.yaml, with every method it
// registers documented under it.
func TestEveryRouteIsInOpenAPISpec(t *testing.T) {
	spec := loadSpec(t)

	routes := map[string][]string{
		"/healthz":                              {"get"},
		"/readyz":                               {"get"},
		"/metrics":                              {"get"},
		"/v1/holds":                             {"get"},
		"/v1/holds/{id}/release":                {"post"},
		"/v1/holds/{id}/reject":                 {"post"},
		"/v1/screening-results":                 {"get"},
		"/v1/screening-results/{id}/invalidate": {"post"},
		"/v1/rescreen-flags":                    {"get"},
		"/v1/rescreen-flags/{id}/resolve":       {"post"},
		"/v1/system/queue":                      {"get"},
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

	sections := map[string]map[string]any{
		"schemas":    spec.Components.Schemas,
		"parameters": spec.Components.Parameters,
		"responses":  spec.Components.Responses,
	}

	for _, match := range refPattern.FindAllStringSubmatch(string(data), -1) {
		section, name := match[1], match[2]
		members, ok := sections[section]
		if !ok {
			t.Errorf("$ref names unknown component section %q", section)
			continue
		}
		if _, ok := members[name]; !ok {
			t.Errorf("$ref #/components/%s/%s does not resolve", section, name)
		}
	}
}

// refPattern matches a local component reference, e.g.
// "#/components/schemas/Hold".
var refPattern = regexp.MustCompile(`#/components/(\w+)/(\w+)`)
