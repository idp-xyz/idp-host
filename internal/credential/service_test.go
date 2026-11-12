package credential

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The HOST's own credential for a downstream audience (ADR-0001 D36): minted once, reused while it is
// good, and minted again before it expires - a credential configured once does not stop working.

func tokenEndpoint(t *testing.T, expiresIn int, grants *[]string) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*grants = append(*grants, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"tok-%d","expires_in":%d,"token_type":"Bearer"}`, n.Add(1), expiresIn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestServiceMintsOnceAndRefreshesBeforeExpiry(t *testing.T) {
	var grants []string
	srv := tokenEndpoint(t, 3600, &grants)
	s := &Service{Exchanger: &Exchanger{TokenEndpoint: srv.URL, ClientID: "idp-host-svc", ClientSecret: "s", Audience: "idp-cmi", HTTP: srv.Client()}}
	first, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != "tok-1" {
		t.Fatalf("first token %q", first)
	}
	for i := 0; i < 5; i++ { // a token that is good is not minted again
		again, err := s.Token(context.Background())
		if err != nil || again != first {
			t.Fatalf("%q %v", again, err)
		}
	}
	if len(grants) != 1 {
		t.Fatalf("%d calls to the token endpoint: %v", len(grants), grants)
	}
	if want := "grant_type=client_credentials"; !contains(grants[0], want) || !contains(grants[0], "client_id=idp-host-svc") || !contains(grants[0], "audience=idp-cmi") {
		t.Fatalf("the HOST did not ask for its own token for the audience: %s", grants[0])
	}
	// close to expiry the next call replaces it, so the holder never uses a token that is about to die
	s.mu.Lock()
	s.tok.ExpiresAt = time.Now().Add(10 * time.Second)
	s.mu.Unlock()
	next, err := s.Token(context.Background())
	if err != nil || next != "tok-2" {
		t.Fatalf("%q %v", next, err)
	}
	if len(grants) != 2 {
		t.Fatalf("%d calls: %v", len(grants), grants)
	}
}

func TestServiceOutlivesAShortLivedToken(t *testing.T) {
	// what broke on the platform: a token with a lifetime shorter than the HOST's. Every write gets a
	// usable one; the endpoint is asked again only when the one held is spent.
	var grants []string
	srv := tokenEndpoint(t, 1, &grants)
	s := &Service{Exchanger: &Exchanger{TokenEndpoint: srv.URL, ClientID: "c", ClientSecret: "s", Audience: "a", HTTP: srv.Client()}, Leeway: 100 * time.Millisecond}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		tok, err := s.Token(context.Background())
		if err != nil || tok == "" {
			t.Fatalf("%q %v", tok, err)
		}
		seen[tok] = true
		time.Sleep(950 * time.Millisecond)
	}
	if len(seen) < 2 {
		t.Fatalf("the same token was used past its lifetime: %v", seen)
	}
}

func TestServiceConcurrentCallersMintOnce(t *testing.T) {
	var grants []string
	srv := tokenEndpoint(t, 3600, &grants)
	s := &Service{Exchanger: &Exchanger{TokenEndpoint: srv.URL, ClientID: "c", ClientSecret: "s", Audience: "a", HTTP: srv.Client()}}
	var wg sync.WaitGroup
	tokens := make([]string, 20)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], _ = s.Token(context.Background())
		}(i)
	}
	wg.Wait()
	for _, tok := range tokens {
		if tok != tokens[0] || tok == "" {
			t.Fatalf("tokens differ: %v", tokens)
		}
	}
	if len(grants) != 1 {
		t.Fatalf("%d calls to the token endpoint for 20 callers", len(grants))
	}
}

func TestServiceReportsARefusalAndKeepsWorking(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	s := &Service{Exchanger: &Exchanger{TokenEndpoint: srv.URL, ClientID: "c", ClientSecret: "s", Audience: "a", HTTP: srv.Client()}}
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("a refused credential must be reported")
	}
	fail.Store(false)
	tok, err := s.Token(context.Background()) // the next call tries again
	if err != nil || tok != "tok" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestServiceWithoutAnExchanger(t *testing.T) {
	var s *Service
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("a nil service must say so")
	}
	if _, err := (&Service{}).Token(context.Background()); err == nil {
		t.Fatal("a service without an exchanger must say so")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
