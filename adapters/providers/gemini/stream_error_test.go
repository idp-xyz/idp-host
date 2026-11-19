package gemini

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idp-host/internal/agui"
	"idp-host/internal/model"
)

// D50: an error event inside a stream that began with 200 is the provider's error, with its own words, not a decode failure.
func TestAStreamErrorIsTheProvidersAndKeepsItsWords(t *testing.T) {
	cases := []struct {
		name, body, code string
		retry            bool
		words            string
	}{
		{"unavailable", `data: {"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE"}}` + "\n\n", model.CodeProviderOverloaded, true, "overloaded."},
		{"other", `data: {"error":{"code":400,"message":"bad field","status":"INVALID_ARGUMENT"}}` + "\n\n", model.CodeProviderError, false, "bad field"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(c.body))
		}))
		var got []model.Delta
		_, err := provider(t, srv).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}},
			func(d model.Delta) { got = append(got, d) })
		srv.Close()
		var me *model.Error
		if !errors.As(err, &me) || me.Code != c.code || me.Retryable != c.retry || !strings.Contains(me.Detail, c.words) {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.words != "" && strings.Contains(err.Error(), c.words) {
			t.Errorf("%s: the provider's words are in Error(): %v", c.name, err)
		}
	}
}

func TestAnUndecodableChunkStaysDecodeAndNeverQuotesThePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {SECRET-PAYLOAD\n\n"))
	}))
	defer srv.Close()
	_, err := provider(t, srv).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != "decode" || me.Retryable || me.Detail == "" || strings.Contains(me.Detail, "SECRET-PAYLOAD") {
		t.Fatalf("%v %+v", err, me)
	}
}
