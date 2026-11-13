package run

import (
	"encoding/json"
	"reflect"
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
		if o, err := ParseOptions(json.RawMessage(raw)); err != nil || !reflect.DeepEqual(o, Options{}) {
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

const testTicket = "eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJib2IifQ.c2ln"

func TestFileOptions(t *testing.T) {
	sum := strings.Repeat("0f", 32)
	raw := `{"idp.host/attachments":[{"ref":"file-1","name":"报价单 v2.docx","mediaType":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","size":48213,"sha256":"` + sum + `","ticket":"` + testTicket + `"}],"idp.host/delivery":{"ticket":"` + testTicket + `"},"app/x":1}`
	o, err := ParseOptions(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Attachments) != 1 || o.Attachments[0].Name != "报价单 v2.docx" || o.Delivery == nil {
		t.Fatalf("%+v", o)
	}
	tk := o.Tickets()
	if tk == nil || len(tk.Read) != 1 || tk.Read[0] != testTicket || tk.Write != testTicket {
		t.Fatalf("%+v", tk)
	}
	stripped := string(WithoutFileOptions(json.RawMessage(raw)))
	if strings.Contains(stripped, "ticket") || strings.Contains(stripped, "attachments") || !strings.Contains(stripped, "app/x") {
		t.Fatalf("the stored forwardedProps keep a ticket: %s", stripped)
	}
	if (Options{}).Tickets() != nil {
		t.Fatal("tickets without file options")
	}
	att := func(field, value string) string {
		m := map[string]any{"ref": "file-1", "name": "a.docx", "mediaType": "text/plain", "size": 1, "sha256": sum, "ticket": testTicket}
		if value == "<delete>" {
			delete(m, field)
		} else {
			m[field] = value
		}
		b, _ := json.Marshal(map[string]any{OptAttachments: []any{m}})
		return string(b)
	}
	bad := []string{
		`{"idp.host/attachments":[]}`,
		`{"idp.host/attachments":{}}`,
		att("ticket", "<delete>"),
		att("ticket", "not a ticket"),
		att("ticket", "a.b.c d"),
		att("name", "line\nbreak.docx"),
		att("name", "../etc/passwd"),
		att("name", ""),
		att("sha256", "xyz"),
		att("ref", "has space"),
		att("mediaType", "docx"),
		att("extra", "1"),
		`{"idp.host/attachments":[` + strings.Repeat(`{"ref":"r","name":"a","mediaType":"text/plain","size":1,"sha256":"`+sum+`","ticket":"`+testTicket+`"},`, 1) + `{"ref":"r","name":"b","mediaType":"text/plain","size":1,"sha256":"` + sum + `","ticket":"` + testTicket + `"}]}`,
		`{"idp.host/delivery":{}}`,
		`{"idp.host/delivery":{"ticket":"` + testTicket + `","maxBytes":1}}`,
		`{"idp.host/delivery":"` + testTicket + `"}`,
	}
	for _, b := range bad {
		if _, err := ParseOptions(json.RawMessage(b)); err == nil {
			t.Errorf("%s accepted", b)
		}
	}
	var many []any
	for i := 0; i <= MaxAttachments; i++ {
		many = append(many, map[string]any{"ref": "f" + string(rune('a'+i)), "name": "a", "mediaType": "text/plain", "size": 1, "sha256": sum, "ticket": testTicket})
	}
	b, _ := json.Marshal(map[string]any{OptAttachments: many})
	if _, err := ParseOptions(b); err == nil {
		t.Errorf("%d attachments accepted", len(many))
	}
}
