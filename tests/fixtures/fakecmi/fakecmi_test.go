package fakecmi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func post(t *testing.T, srv *httptest.Server, path, bearer string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The fake follows idp.telos.cmi 1.7.0: requests are checked, scope and sources narrow, find's hits carry hitCount.
func TestTheFakeFollowsThe170Requests(t *testing.T) {
	s := New(Doc{ID: "d", Title: "T", Source: "ops", Blocks: []Block{{Text: "alpha beta"}}})
	srv := s.Start()
	defer srv.Close()
	if st, _ := post(t, srv, "/v1/context/read", "member:a", map[string]any{"documentId": "d", "purpose": "x", "sources": []string{"ops"}, "scope": map[string]any{"kind": "subject", "id": "a"}}); st != 200 {
		t.Errorf("a documented request: %d", st)
	}
	for name, body := range map[string]map[string]any{
		"unknown key":      {"documentId": "d", "limit": 3},
		"unknown locator":  {"documentId": "d", "locator": map[string]any{"Page": 1}},
		"scope of another": {"documentId": "d", "scope": map[string]any{"kind": "subject", "id": "b"}},
		"outside sources":  {"documentId": "d", "sources": []string{"hr"}},
	} {
		want := 404
		if name == "unknown key" || name == "unknown locator" {
			want = 422
		}
		if st, _ := post(t, srv, "/v1/context/read", "member:a", body); st != want {
			t.Errorf("%s: %d, want %d", name, st, want)
		}
	}
	_, out := post(t, srv, "/v1/context/find", "member:a", map[string]any{"documentId": "d", "text": "BETA"})
	it := out["package"].(map[string]any)["items"].([]any)[0].(map[string]any)
	if it["match"] != "exact" || it["hitCount"] != float64(1) || len(it["hits"].([]any)) != 1 {
		t.Errorf("%v", it)
	}
}
