package openai

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
		{"openai-compatible, after a chunk", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n" + `data: {"error":{"message":"Server hiccup, try later","type":"server_error","code":500}}` + "\n\n", model.CodeProviderServer, true, "Server hiccup"},
		{"openai-compatible, first event", `data: {"error":{"message":"bad things","type":"invalid_request_error","code":"bad"}}` + "\n\n", model.CodeProviderError, false, "bad things"},
		{"openai-compatible, context", `data: {"error":{"message":"the request exceeds the available context size","type":"exceed_context_size_error","code":400}}` + "\n\n", model.CodeContextLength, false, "context size"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(c.body))
		}))
		var got []model.Delta
		_, err := provider(t, srv, true).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}},
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
	_, err := provider(t, srv, true).Complete(context.Background(), model.Request{Model: "m", Messages: []agui.Message{{ID: "u1", Role: agui.RoleUser, Content: "hi"}}}, nil)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != "decode" || me.Retryable || me.Detail == "" || strings.Contains(me.Detail, "SECRET-PAYLOAD") {
		t.Fatalf("%v %+v", err, me)
	}
}
