package httpapi

import (
	"net/http"
	"os"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"
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

// TestEveryRouteIsInOpenAPISpec walks the REAL router (chi.Walk, not a
// hand-maintained list) so route coverage can never silently drift as
// routes are added -- a strictly stronger form of every sibling
// component's own "full route coverage in the OpenAPI spec, verified
// by test" acceptance bar.
func TestEveryRouteIsInOpenAPISpec(t *testing.T) {
	s := &Server{}
	router := NewRouter(s)
	spec := loadSpec(t)

	err := chi.Walk(router.(chi.Router), func(method, route string, handler http.Handler, middlewares ...func(http.Handler) http.Handler) error {
		operations, ok := spec.Paths[route]
		if !ok {
			t.Errorf("route %q is not present as a path key in docs/openapi.yaml", route)
			return nil
		}
		if _, ok := operations[toLowerMethod(method)]; !ok {
			t.Errorf("route %q is documented, but its %s operation is not", route, method)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
}

func toLowerMethod(method string) string {
	switch method {
	case http.MethodGet:
		return "get"
	case http.MethodPost:
		return "post"
	case http.MethodPut:
		return "put"
	case http.MethodDelete:
		return "delete"
	case http.MethodPatch:
		return "patch"
	default:
		return method
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
