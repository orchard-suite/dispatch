package server

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOrreryYAMLNamesFilesThatExist fails while orrery.yaml at the repository
// root names an icon or an API spec the repository does not hold, or an API
// whose spec is not openapi.yaml.
func TestOrreryYAMLNamesFilesThatExist(t *testing.T) {
	raw, err := os.ReadFile("../../orrery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Service string `yaml:"service"`
		Icon    struct {
			File string `yaml:"file"`
		} `yaml:"icon"`
		Provides []struct {
			ID   string `yaml:"id"`
			Spec string `yaml:"spec"`
		} `yaml:"provides"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("orrery.yaml: %v", err)
	}
	if want := "github/orchard-suite/dispatch/cmd/dispatch"; d.Service != want {
		t.Errorf("service = %q, want %q", d.Service, want)
	}
	files := []string{d.Icon.File}
	if d.Icon.File == "" {
		t.Error("orrery.yaml names no icon file")
	}
	if len(d.Provides) == 0 {
		t.Error("orrery.yaml provides no API")
	}
	for _, p := range d.Provides {
		if p.Spec != "openapi.yaml" {
			t.Errorf("API %q: spec = %q, want openapi.yaml", p.ID, p.Spec)
		}
		files = append(files, p.Spec)
	}
	for _, f := range files {
		if f == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join("../..", f)); err != nil {
			t.Errorf("orrery.yaml names %q: %v", f, err)
		}
	}
}
