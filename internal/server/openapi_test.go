package server

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIMatchesRoutes fails while openapi.yaml at the repository root
// and the routes the API port registers differ.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	methods := []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
	var documented []string
	for path, item := range spec.Paths {
		for key := range item {
			if slices.Contains(methods, key) {
				documented = append(documented, strings.ToUpper(key)+" "+path)
			}
		}
	}
	var registered []string
	for _, rt := range (&Server{}).apiRoutes() {
		registered = append(registered, rt.pattern)
	}
	slices.Sort(documented)
	slices.Sort(registered)
	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("update openapi.yaml: the API port serves %q and openapi.yaml does not describe it", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(registered, d) {
			t.Errorf("update openapi.yaml: it describes %q and the API port does not serve it", d)
		}
	}
}
