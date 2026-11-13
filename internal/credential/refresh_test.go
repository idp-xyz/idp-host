package credential

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// D38: a session-bound exchange asks for a refresh token and keeps it; a refresh renews the credential, keeps the
// refresh token when the server does not rotate it, and reports a refused refresh token as the end of the session.
func TestSessionBoundExchangeAndRefresh(t *testing.T) {
	var forms []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f := map[string]string{}
		for k := range r.PostForm {
			f[k] = r.PostForm.Get(k)
		}
		forms = append(forms, f)
		switch {
		case f["grant_type"] == "urn:ietf:params:oauth:grant-type:token-exchange":
			_, _ = io.WriteString(w, `{"access_token":"gw-1","expires_in":600,"refresh_token":"rt-1"}`)
		case f["grant_type"] == "refresh_token" && f["refresh_token"] == "rt-1":
			_, _ = io.WriteString(w, `{"access_token":"gw-2","expires_in":600}`)
		default:
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Session not active"}`)
		}
	}))
	defer srv.Close()
	e := &Exchanger{TokenEndpoint: srv.URL, ClientID: "idp-host", ClientSecret: "s", Audience: "gw", SessionBound: true, HTTP: srv.Client()}
	tok, err := e.Exchange(context.Background(), "subject", "")
	if err != nil || tok.AccessToken != "gw-1" || tok.RefreshToken != "rt-1" || forms[0]["requested_token_type"] != "urn:ietf:params:oauth:token-type:refresh_token" {
		t.Fatalf("%+v %v %v", tok, err, forms[0])
	}
	tok, err = e.Refresh(context.Background(), "rt-1")
	if err != nil || tok.AccessToken != "gw-2" || tok.RefreshToken != "rt-1" || forms[1]["client_id"] != "idp-host" || forms[1]["client_secret"] != "s" {
		t.Fatalf("%+v %v %v", tok, err, forms[1])
	}
	if _, err := e.Refresh(context.Background(), "rt-old"); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("a refused refresh token: %v", err)
	}
	if _, err := e.Refresh(context.Background(), ""); !errors.Is(err, ErrSessionEnded) {
		t.Fatalf("no refresh token: %v", err)
	}
	e.SessionBound = false
	_, _ = e.Exchange(context.Background(), "subject", "")
	if forms[len(forms)-1]["requested_token_type"] != "urn:ietf:params:oauth:token-type:access_token" {
		t.Fatal("a plain exchange asked for a refresh token")
	}
	// an unreachable server is not the end of the session
	down := &Exchanger{TokenEndpoint: "http://127.0.0.1:9/token", ClientID: "x", ClientSecret: "y", HTTP: srv.Client()}
	if _, err := down.Refresh(context.Background(), "rt-1"); err == nil || errors.Is(err, ErrSessionEnded) {
		t.Fatalf("%v", err)
	}
}
