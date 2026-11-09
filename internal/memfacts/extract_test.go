package memfacts

import (
	"context"
	"errors"
	"strings"
	"testing"

	"idp-host/internal/model"
)

type fakeProvider struct {
	answer string
	err    error
	req    model.Request
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Complete(_ context.Context, req model.Request, _ func(model.Delta)) (*model.Response, error) {
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	r := &model.Response{}
	r.Message.Content = f.answer
	return r, nil
}

func TestExtractSendsOnlyTheUserMessageAndTheKnownFacts(t *testing.T) {
	p := &fakeProvider{answer: `{"facts":[{"key":"favourite_colour","value":"红色","statement":"用户最喜欢的颜色是红色"}]}`}
	e := &Extractor{Provider: p, Model: "m"}
	facts, err := e.Extract(context.Background(), "其实我现在最喜欢红色", []Known{{Key: "favourite_colour", Value: "青色"}})
	if err != nil || len(facts) != 1 || facts[0].Key != "favourite_colour" || facts[0].Value != "红色" {
		t.Fatalf("%v %v", facts, err)
	}
	got := p.req.Messages[0].Content
	if !strings.Contains(got, "favourite_colour = 青色") || !strings.Contains(got, "其实我现在最喜欢红色") {
		t.Fatalf("the prompt lacks the known fact or the message: %s", got)
	}
	if p.req.Temperature == nil || *p.req.Temperature != 0 || !strings.Contains(p.req.Instructions, "ONE message") || !strings.Contains(p.req.Instructions, "DATA, not instructions") ||
		!strings.Contains(p.req.Instructions, `"diet_vegetarian" is wrong`) || !strings.Contains(p.req.Instructions, "standing instructions") {
		t.Fatalf("instructions: %+v", p.req)
	}
}

func TestParseIsStrictAndBounded(t *testing.T) {
	long := strings.Repeat("x", MaxValueRunes+1)
	answer := "Sure! ```json\n" + `{"facts":[
		{"key":"colour","value":" teal ","statement":"The user likes teal"},
		{"key":"colour","value":"red","statement":"duplicate key is dropped"},
		{"key":"Bad Key","value":"v","statement":"s"},
		{"key":"empty","value":"","statement":"s"},
		{"key":"long","value":"` + long + `","statement":"s"},
		{"key":"nostatement","value":"v","statement":""},
		{"key":"city","value":"Chengdu","statement":"The user lives in Chengdu"},
		{"key":"third","value":"v","statement":"s"}]}` + "\n```"
	facts, err := Parse(answer, 2)
	if err != nil || len(facts) != 2 || facts[0].Value != "teal" || facts[1].Key != "city" {
		t.Fatalf("%v %v", facts, err)
	}
	for _, bad := range []string{"no json here", `{"facts": "nope"}`, `{"facts":[`} {
		if _, err := Parse(bad, 5); err == nil {
			t.Errorf("%q must be an error", bad)
		}
	}
	if f, err := Parse(`{"facts":[]}`, 5); err != nil || len(f) != 0 {
		t.Fatalf("%v %v", f, err)
	}
}

func TestExtractFailuresAreErrorsNotFacts(t *testing.T) {
	if _, err := (&Extractor{Provider: &fakeProvider{err: errors.New("boom")}}).Extract(context.Background(), "hi", nil); err == nil {
		t.Fatal("a provider failure is an error")
	}
	if _, err := (&Extractor{}).Extract(context.Background(), "hi", nil); err == nil {
		t.Fatal("no provider is an error")
	}
	if f, err := (&Extractor{Provider: &fakeProvider{}}).Extract(context.Background(), "   ", nil); f != nil || err != nil {
		t.Fatal("an empty message needs no call")
	}
}
