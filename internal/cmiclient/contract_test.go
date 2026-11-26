package cmiclient

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestCMIContractPin is the consumer side of idp.telos.cmi (idp-telos-go ADR-0001 T3): the copy in
// tests/contracts/idp.telos.cmi/<version> is byte-identical to the registered one (tools/platform.py
// matrix), every field the HOST reads exists in the contract, the ones it relies on are required,
// and the published examples decode into the HOST's types.
func TestCMIContractPin(t *testing.T) {
	dir := "../../tests/contracts/idp.telos.cmi/1.7.0/"
	var schema struct {
		Defs map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	raw, err := os.ReadFile(dir + "schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	optional := map[string]bool{"title": true, "validUntil": true, "match": true, "locator": true, "hits": true, "hitCount": true} // read when present
	check := func(def string, v any) {
		d := schema.Defs[def]
		required := map[string]bool{}
		for _, r := range d.Required {
			required[r] = true
		}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if _, ok := d.Properties[name]; !ok {
				t.Errorf("the HOST reads %s.%s, which idp.telos.cmi does not define", def, name)
			} else if !required[name] && !optional[name] {
				t.Errorf("the HOST relies on %s.%s, which idp.telos.cmi does not require", def, name)
			}
		}
	}
	check("ContextItem", Item{})
	// what is read inside the nested objects: the locator (page, slide, sheet, range, headings, table) and a hit (offset, length)
	nested := func(path string, v any, props map[string]json.RawMessage) {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if _, ok := props[name]; !ok {
				t.Errorf("the HOST reads %s.%s, which idp.telos.cmi does not define", path, name)
			}
		}
	}
	var item struct {
		Properties struct {
			Locator struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"locator"`
			Hits struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"hits"`
			Match struct {
				Enum []string `json:"enum"`
			} `json:"match"`
		} `json:"properties"`
	}
	var whole struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	_ = json.Unmarshal(raw, &whole)
	if err := json.Unmarshal(whole.Defs["ContextItem"], &item); err != nil {
		t.Fatal(err)
	}
	nested("ContextItem.locator", Locator{}, item.Properties.Locator.Properties)
	nested("ContextItem.hits", Hit{}, item.Properties.Hits.Items.Properties)
	if !slices.Contains(item.Properties.Match.Enum, "exact") {
		t.Error("find's match value exact is not in the contract")
	}
	check("Verification", Verification{})
	var examples struct {
		Examples []struct {
			Definition string          `json:"definition"`
			Value      json.RawMessage `json:"value"`
		} `json:"examples"`
	}
	raw, err = os.ReadFile(dir + "examples.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &examples); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, x := range examples.Examples {
		switch x.Definition {
		case "ContextPackage":
			var p struct {
				Items []Item `json:"items"`
			}
			if err := json.Unmarshal(x.Value, &p); err != nil || len(p.Items) == 0 || p.Items[0].ID == "" || p.Items[0].ObservedAt.IsZero() {
				t.Errorf("a published ContextPackage does not decode into the HOST's items: %v", err)
			}
		case "Verification":
			var v Verification
			if err := json.Unmarshal(x.Value, &v); err != nil || v.PackageID == "" {
				t.Errorf("a published Verification does not decode: %+v %v", v, err)
			}
		default:
			continue
		}
		seen[x.Definition]++
	}
	if seen["ContextPackage"] == 0 || seen["Verification"] == 0 {
		t.Fatalf("examples missing: %v", seen)
	}
}
