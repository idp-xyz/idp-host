package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIMatchesRouter: every route the router registers is documented and nothing documented
// is unrouted.
func TestOpenAPIMatchesRouter(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ /[^"]*)"`)
	var routed []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		routed = append(routed, m[1])
	}
	sort.Strings(routed)

	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var documented []string
	for path, ops := range doc.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete":
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(documented)
	if strings.Join(routed, "\n") != strings.Join(documented, "\n") {
		t.Fatalf("router and api/openapi.yaml differ\nrouted:\n%s\ndocumented:\n%s", strings.Join(routed, "\n"), strings.Join(documented, "\n"))
	}
	if len(routed) < 25 {
		t.Fatalf("only %d routes found in api.go", len(routed))
	}
}
