package api

// D51: the gateway's marker counts at run creation wherever the operator's list counts: the release's own tools, its
// allow list of names or "*", the sub-agents' releases; a catalogue the HOST cannot read counts as outward.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idp-host/internal/agent"
	"idp-host/internal/credential"
	"idp-host/internal/mcpclient"
	"idp-host/internal/run"
	"idp-host/tests/fixtures/fakemcp"
)

// catalogues answers per gateway and counts the questions; a gateway that maps to nil cannot be read.
type catalogues struct {
	by    map[string]map[string]bool
	asked []string
}

func (c *catalogues) look(gateway string) (map[string]bool, error) {
	c.asked = append(c.asked, gateway)
	if cat := c.by[gateway]; cat != nil {
		return cat, nil
	}
	return nil, errors.New("unreachable")
}

func TestTheGatewaysMarkerMakesAReleaseOutwardWithoutAnyOperatorPattern(t *testing.T) {
	cat := map[string]bool{"partner__lookup_company": true, "partner__rates": true, "sum": false, "echo": false}
	p := FilePolicy{Local: map[string]bool{"onprem": true}} // the operator lists nothing
	for name, c := range map[string]struct {
		allow []string
		by    map[string]map[string]bool
		trig  string
		via   string
		asked int
	}{
		"allows a marked tool":                               {[]string{"echo", "partner__lookup_company"}, map[string]map[string]bool{"edge": cat}, "external-tools", "marked by the gateway as sending content outside the enterprise: partner__lookup_company", 1},
		"allows only unmarked tools (the control)":           {[]string{"echo", "sum"}, map[string]map[string]bool{"edge": cat}, "", "", 1},
		"allows a marked tool the gateway does not list yet": {[]string{"echo", "partner__new"}, map[string]map[string]bool{"edge": cat}, "", "", 1},
		"allows every tool and one is marked":                {[]string{agent.AllowAll}, map[string]map[string]bool{"edge": cat}, "external-tools", "partner__lookup_company, partner__rates", 1},
		"allows every tool and none is marked":               {[]string{agent.AllowAll}, map[string]map[string]bool{"edge": {"sum": false}}, "", "", 1},
		"the catalogue cannot be read":                       {[]string{"echo"}, map[string]map[string]bool{}, "external-tools", "gateway edge: its tool catalogue could not be read", 1},
		"no tools asks nobody":                               {nil, map[string]map[string]bool{"edge": cat}, "", "", 0},
	} {
		cs := &catalogues{by: c.by}
		o := p.outward(context.Background(), "t", docWithTools("onprem", "edge", c.allow...), cs.look)
		if o.Trigger != c.trig || !strings.Contains(strings.Join(o.Via, "\n"), c.via) || len(cs.asked) != c.asked || (c.via == "") != (len(o.Via) == 0) {
			t.Errorf("%s: trigger %q via %q asked %v", name, o.Trigger, o.Via, cs.asked)
		}
	}
	// the operator's pattern already says outward: the gateway is not asked, and no reason is given for a marker
	cs := &catalogues{by: map[string]map[string]bool{"edge": cat}}
	p.ExternalTools = map[string][]string{"edge": {"web__*"}}
	if o := p.outward(context.Background(), "t", docWithTools("onprem", "edge", "web__search"), cs.look); o.Trigger != "external-tools" || len(cs.asked) != 0 || len(o.Via) != 0 {
		t.Errorf("%+v %v", o, cs.asked)
	}
	// a nil Catalogue is not consulted: only the operator's list counts
	if o := (FilePolicy{Local: map[string]bool{"onprem": true}}).outward(context.Background(), "t", docWithTools("onprem", "edge", "partner__lookup_company"), nil); o.Trigger != "" {
		t.Errorf("%q", o.Trigger)
	}
	// a skill's tools are in the effective policy, so a marked tool of a skill counts
	d := docWithTools("onprem", "edge", "echo")
	d.Definition.Skills = []agent.SkillRef{{Name: "research"}}
	d.Skills = []agent.EmbeddedSkill{{Name: "research", Definition: agent.SkillDefinition{Tools: []string{"partner__rates"}}}}
	if o := (FilePolicy{}).outward(context.Background(), "t", d, (&catalogues{by: map[string]map[string]bool{"edge": cat}}).look); !o.Tools {
		t.Error("a skill's marked tool was not seen")
	}
}

func TestTheMarkerAndAnUnreadableCatalogueCountForSubagents(t *testing.T) {
	cat := map[string]bool{"partner__lookup_company": true, "sum": false}
	p := FilePolicy{Local: map[string]bool{"onprem": true}}
	for name, c := range map[string]struct {
		subs tree
		by   map[string]map[string]bool
		trig string
		via  string
	}{
		"a sub-agent allows a marked tool":                  {tree{"d-web": node("onprem", "edge", []string{"partner__lookup_company"}, 0)}, map[string]map[string]bool{"edge": cat}, "subagent", "sub-agent web: may call an external tool (marked by the gateway: partner__lookup_company)"},
		"a sub-agent allows an unmarked tool (the control)": {tree{"d-web": node("onprem", "edge", []string{"sum"}, 0)}, map[string]map[string]bool{"edge": cat}, "", ""},
		"two levels down":                                   {tree{"d-mid": node("onprem", "edge", nil, 2, "web"), "d-web": node("onprem", "edge", []string{agent.AllowAll}, 0)}, map[string]map[string]bool{"edge": cat}, "subagent", "sub-agent mid/web: may call an external tool (marked by the gateway"},
		"the catalogue cannot be read":                      {tree{"d-web": node("onprem", "edge", []string{"sum"}, 0)}, map[string]map[string]bool{}, "subagent", "the catalogue of gateway edge could not be read"},
	} {
		root := node("onprem", "edge", nil, 1, "mid")
		if name != "two levels down" {
			root = node("onprem", "edge", nil, 0, "web")
		}
		pol := p
		pol.Release = c.subs.release
		o := pol.outward(context.Background(), "t", root, (&catalogues{by: c.by}).look)
		if o.Trigger != c.trig || !strings.Contains(strings.Join(o.Via, "\n"), c.via) || (c.via == "") != (len(o.Via) == 0) {
			t.Errorf("%s: trigger %q via %q", name, o.Trigger, o.Via)
		}
	}
	// the events of a withheld file say that it was the marker
	o := Outward{Trigger: "external-tools", Tools: true, Via: []string{"tools marked by the gateway as sending content outside the enterprise: partner__lookup_company"}}
	ev := withheldEvents([]run.Attachment{{Ref: "f", Name: "a.txt", Sensitivity: "confidential"}}, "internal", o)
	if len(ev) != 1 || !strings.Contains(fmt.Sprint(ev[0]), "partner__lookup_company") {
		t.Errorf("%v", ev)
	}
}

// D51: the catalogue is read with the caller's own credential, once per gateway, and anything that goes wrong is an error.
func TestTheCatalogueIsReadWithTheCallersCredentialOncePerGateway(t *testing.T) {
	fake := fakemcp.New()
	fake.RequireBearer = "delegated:carol"
	fake.Tools[0].Meta = map[string]any{mcpclient.ExternalMetaKey: true}
	srv := httptest.NewServer(fake.Handler())
	defer srv.Close()
	client := &mcpclient.Client{HTTP: mcpclient.NewHTTPClient(5*time.Second, nil), Info: mcpclient.ClientInfo{Name: "idp-host", Version: "test"}}
	a := &API{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Gateways: map[string]GatewayAuth{
		"edge":   {Endpoint: srv.URL + "/mcp", Client: client},
		"static": {Endpoint: srv.URL + "/mcp", Client: client, StaticBearer: "delegated:carol"},
		"bare":   {},
	}}
	look := a.catalogue(context.Background(), "edge", &credential.Token{AccessToken: "delegated:carol"})
	for i := 0; i < 2; i++ {
		got, err := look("edge")
		if err != nil || !got["sum"] || got["echo"] {
			t.Fatalf("%v %v", got, err)
		}
	}
	if fake.Lists() != 1 {
		t.Errorf("%d tools/list calls for one gateway in one creation", fake.Lists())
	}
	if got := fake.Bearers(); len(got) != 1 || got[0] != "delegated:carol" {
		t.Errorf("%v", got)
	}
	if _, err := look("static"); err != nil { // another gateway with a static credential of its own
		t.Error(err)
	}
	if _, err := look("bare"); err == nil {
		t.Error("a gateway the HOST has no client for was read")
	}
	if _, err := look("unknown"); err == nil {
		t.Error("a gateway that is not configured was read")
	}
	// another gateway than the run's, with only the run's delegated credential: no credential, so unknown
	if _, err := a.catalogue(context.Background(), "other", &credential.Token{AccessToken: "x"})("edge"); err == nil {
		t.Error("the delegated credential of one gateway was sent to another")
	}
	// a credential the gateway refuses, and a gateway that is gone
	if _, err := a.catalogue(context.Background(), "edge", &credential.Token{AccessToken: "wrong"})("edge"); err == nil {
		t.Error("a refused credential gave a catalogue")
	}
	srv.Close()
	if _, err := a.catalogue(context.Background(), "edge", &credential.Token{AccessToken: "delegated:carol"})("edge"); err == nil {
		t.Error("an unreachable gateway gave a catalogue")
	}
}
