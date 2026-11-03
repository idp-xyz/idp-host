package run

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseOptions(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)
	o, err := ParseOptions(json.RawMessage(`{"idp.host/executionRef":"execution:abc","idp.host/toolExecution":"caller","idp.host/releaseDigest":"` + digest + `","idp.host/maxOutputTokens":512,"app/other":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if o.ExecutionRef != "execution:abc" || o.ToolExecution != ToolExecutionCaller || o.ReleaseDigest != digest || o.MaxOutputTokens != 512 {
		t.Fatalf("%+v", o)
	}
	for _, raw := range []string{"", "null", "{}", `{"other":1}`} {
		if o, err := ParseOptions(json.RawMessage(raw)); err != nil || o != (Options{}) {
			t.Errorf("%q: %+v %v", raw, o, err)
		}
	}
	if o, _ := ParseOptions(json.RawMessage(`{"idp.host/toolExecution":"server"}`)); o.ToolExecution != "" {
		t.Fatal("server must normalize to the default")
	}
	bad := []string{
		`[]`,
		`{"idp.host/unknown":1}`,
		`{"idp.host/toolExecution":"both"}`,
		`{"idp.host/releaseDigest":"sha256:xyz"}`,
		`{"idp.host/maxOutputTokens":0}`,
		`{"idp.host/maxOutputTokens":"512"}`,
		`{"idp.host/executionRef":""}`,
		`{"idp.host/executionRef":"has space"}`,
	}
	for _, raw := range bad {
		if _, err := ParseOptions(json.RawMessage(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
