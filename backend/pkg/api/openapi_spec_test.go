package api

import (
	"encoding/json"
	"testing"
)

// TestOpenAPISpecIsValidJSON guards the hand-maintained spec: nothing else
// validates it, so a stray comma would only show up in a client.
func TestOpenAPISpecIsValidJSON(t *testing.T) {
	var spec map[string]any
	if err := json.Unmarshal(getOpenAPISpec(), &spec); err != nil {
		t.Fatalf("openapi spec is not valid JSON: %v", err)
	}
	if spec["openapi"] == "" {
		t.Fatal("spec is missing the openapi version")
	}
}

func TestOpenAPISpecNetworkContract(t *testing.T) {
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name   string `json:"name"`
					Schema struct {
						Type    string   `json:"type"`
						Minimum *float64 `json:"minimum"`
						Maximum *float64 `json:"maximum"`
					} `json:"schema"`
				} `json:"parameters"`
				Responses map[string]any `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(getOpenAPISpec(), &spec); err != nil {
		t.Fatalf("openapi spec is not valid JSON: %v", err)
	}
	path, ok := spec.Paths["/api/sessions/{id}/network"]
	if !ok {
		t.Fatal("network path is missing from the spec")
	}
	for _, code := range []string{"200", "400", "403", "404", "409", "502", "504"} {
		if _, ok := path.Get.Responses[code]; !ok {
			t.Errorf("network path is missing response %s", code)
		}
	}
	for _, p := range path.Get.Parameters {
		if p.Name != "limit" {
			continue
		}
		if p.Schema.Type != "integer" || p.Schema.Minimum == nil || *p.Schema.Minimum != 1 ||
			p.Schema.Maximum == nil || *p.Schema.Maximum != 1000 {
			t.Errorf("limit schema = %+v, want integer 1..1000", p.Schema)
		}
	}
}

func TestOpenAPISpecGraphContract(t *testing.T) {
	type schema struct {
		Type    string   `json:"type"`
		Minimum *float64 `json:"minimum"`
		Maximum *float64 `json:"maximum"`
	}
	type parameter struct {
		Name   string `json:"name"`
		Schema schema `json:"schema"`
	}
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Parameters []parameter    `json:"parameters"`
				Responses  map[string]any `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(getOpenAPISpec(), &spec); err != nil {
		t.Fatalf("openapi spec is not valid JSON: %v", err)
	}

	path, ok := spec.Paths["/api/sessions/{id}/graph.png"]
	if !ok {
		t.Fatal("graph path is missing from the spec")
	}

	for _, code := range []string{"200", "400", "403", "404", "409", "429", "502", "504"} {
		if _, ok := path.Get.Responses[code]; !ok {
			t.Errorf("graph path is missing response %s", code)
		}
	}

	params := make(map[string]schema)
	for _, p := range path.Get.Parameters {
		params[p.Name] = p.Schema
	}
	for _, name := range []string{"width", "height"} {
		s, ok := params[name]
		if !ok {
			t.Errorf("graph path is missing parameter %q", name)
			continue
		}
		if s.Type != "integer" {
			t.Errorf("%s type = %q, want integer", name, s.Type)
		}
		if s.Minimum == nil || *s.Minimum != 320 {
			t.Errorf("%s minimum = %v, want 320", name, s.Minimum)
		}
		if s.Maximum == nil || *s.Maximum != 8000 {
			t.Errorf("%s maximum = %v, want 8000", name, s.Maximum)
		}
	}
}
