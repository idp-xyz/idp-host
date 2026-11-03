package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExpositionFormat(t *testing.T) {
	m := NewMetrics()
	c := m.Counter("idp_test_total", "A counter.", "method", "outcome")
	c.Inc("tools/call", "OK")
	c.Inc("tools/call", "OK")
	c.Add(3, "prompts/get", `quo"te\n`)
	c.Inc("wrong-arity") // ignored
	h := m.Histogram("idp_test_seconds", "A histogram.", []float64{0.1, 1}, "method")
	h.Observe(0.05, "x")
	h.Observe(0.5, "x")
	h.Observe(5, "x")
	m.GaugeFunc("idp_test_gauge", "A gauge.", []string{"target"}, func() []LabeledValue {
		return []LabeledValue{{Labels: []string{"a"}, Value: 1}, {Labels: []string{"b"}, Value: 0}}
	})
	var b strings.Builder
	m.Write(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE idp_test_total counter\n",
		`idp_test_total{method="prompts/get",outcome="quo\"te\\n"} 3` + "\n",
		`idp_test_total{method="tools/call",outcome="OK"} 2` + "\n",
		"# TYPE idp_test_seconds histogram\n",
		`idp_test_seconds_bucket{method="x",le="0.1"} 1` + "\n",
		`idp_test_seconds_bucket{method="x",le="1"} 2` + "\n",
		`idp_test_seconds_bucket{method="x",le="+Inf"} 3` + "\n",
		`idp_test_seconds_sum{method="x"} 5.55` + "\n",
		`idp_test_seconds_count{method="x"} 3` + "\n",
		`idp_test_gauge{target="a"} 1` + "\n",
		`idp_test_gauge{target="b"} 0` + "\n",
		"# TYPE process_start_time_seconds gauge\n",
		"# TYPE go_goroutines gauge\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wrong-arity") {
		t.Error("a sample with the wrong label arity was recorded")
	}
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain; version=0.0.4") || !strings.Contains(string(body), `idp_test_total{method="tools/call",outcome="OK"} 2`) {
		t.Fatalf("handler: %s %q", resp.Header.Get("Content-Type"), body)
	}
}

func TestHTTPMetricsCountByRouteAndStatus(t *testing.T) {
	m := NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/servers/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	srv := httptest.NewServer(HTTPMetrics(m, "idp_mcp_registry", mux))
	defer srv.Close()
	for i := 0; i < 3; i++ {
		resp, _ := http.Get(srv.URL + "/api/v1/servers/x")
		resp.Body.Close()
	}
	resp, _ := http.Get(srv.URL + "/nope")
	resp.Body.Close()
	var b strings.Builder
	m.Write(&b)
	out := b.String()
	for _, want := range []string{
		`idp_mcp_registry_http_requests_total{route="GET /api/v1/servers/{id}",status="404"} 3`,
		`idp_mcp_registry_http_requests_total{route="unmatched",status="404"} 1`,
		`idp_mcp_registry_http_request_duration_seconds_count{route="GET /api/v1/servers/{id}"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/servers/x") {
		t.Error("a concrete path leaked into a label (cardinality, privacy)")
	}
}
