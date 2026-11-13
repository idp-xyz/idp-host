// Package credential keeps the delegated gateway token of a run (ADR-0001 D8): obtained from the
// authorization server by RFC 8693 token exchange when the run is created, stored encrypted with
// the HOST's key, decrypted only by the worker that executes the run's tool calls.
package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Box encrypts and decrypts with AES-256-GCM.
type Box struct{ aead cipher.AEAD }

// NewBox takes a 32-byte key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("credential key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain (nonce prepended).
func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return append(nonce, b.aead.Seal(nil, nonce, plain, nil)...), nil
}

// Open decrypts a sealed value.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed value too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}

// Exchanger performs RFC 8693 token exchange: the user's access token (subject) is exchanged, by
// the HOST acting as an OAuth client, for a token whose audience is the gateway resource and whose
// act claim names the HOST (delegation). The authorization server decides whether the user may be
// represented by this client (Keycloak: may_act on the subject token).
type Exchanger struct {
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	Audience      string // the gateway's client / resource identifier at the authorization server
	// SessionBound asks for a refresh token bound to the user's session alongside the delegated token (D38), so that
	// a run can renew its credential until the user signs out or the session reaches its limits.
	SessionBound bool
	HTTP         *http.Client
}

// Token is an exchanged token.
type Token struct {
	AccessToken  string
	ExpiresAt    time.Time
	RefreshToken string // only from a session-bound exchange or a refresh
}

// ErrSessionEnded: the authorization server no longer renews the credential (the user signed out, the session idled
// out or reached its maximum, or it was revoked).
var ErrSessionEnded = errors.New("the user's session has ended")

// Exchange trades subjectToken for a delegated token. actorToken, when non-empty, is sent as the
// actor_token (delegation profile); otherwise the client credentials identify the actor.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken, actorToken string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	if e.SessionBound {
		form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token")
	} else {
		form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	}
	if e.Audience != "" {
		form.Set("audience", e.Audience)
	}
	if actorToken != "" {
		form.Set("actor_token", actorToken)
		form.Set("actor_token_type", "urn:ietf:params:oauth:token-type:access_token")
	}
	return e.post(ctx, form)
}

// Refresh renews a session-bound credential. ErrSessionEnded when the server refuses the refresh token itself.
func (e *Exchanger) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	if refreshToken == "" {
		return nil, ErrSessionEnded
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	tok, err := e.post(ctx, form)
	if err != nil {
		var re *refused
		if errors.As(err, &re) && (re.code == "invalid_grant" || re.status == http.StatusUnauthorized) {
			return nil, fmt.Errorf("%w (%s)", ErrSessionEnded, re.code)
		}
		return nil, err
	}
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

// refused is the authorization server's refusal.
type refused struct {
	status int
	code   string
}

func (r *refused) Error() string {
	return fmt.Sprintf("token exchange refused (HTTP %d%s)", r.status, suffix(r.code))
}

// ClientCredentials obtains the HOST's own token: the actor_token of a delegation when the server
// requires one, and the HOST's own credential for a downstream audience (D36). The audience is sent
// for servers that take it there; Keycloak puts it in from the client's own mapper and ignores it.
func (e *Exchanger) ClientCredentials(ctx context.Context) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if e.Audience != "" {
		form.Set("audience", e.Audience)
	}
	return e.post(ctx, form)
}

func (e *Exchanger) post(ctx context.Context, form url.Values) (*Token, error) {
	form.Set("client_id", e.ClientID)
	form.Set("client_secret", e.ClientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		return nil, &refused{status: resp.StatusCode, code: e.Error}
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return nil, errors.New("token endpoint returned no access token")
	}
	exp := time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	if out.ExpiresIn <= 0 {
		exp = time.Now().Add(5 * time.Minute)
	}
	return &Token{AccessToken: out.AccessToken, ExpiresAt: exp, RefreshToken: out.RefreshToken}, nil
}

func suffix(code string) string {
	if code == "" {
		return ""
	}
	return ", " + code
}

// Service is the HOST's own credential for a downstream audience (ADR-0001 D36): a token of its client
// identity, minted on first use and again before it expires. A credential configured once therefore does
// not stop working - unlike a token handed to the HOST at deployment time, which outlives nothing but its
// own lifetime. Safe for concurrent use.
type Service struct {
	Exchanger *Exchanger
	// Leeway is how long before expiry a token is replaced; default 30s.
	Leeway time.Duration
	mu     sync.Mutex
	tok    *Token
}

// Token returns a token that is valid now, minting one when there is none or the one held is close to
// its expiry. A failure leaves the held token in place: the next call tries again.
func (s *Service) Token(ctx context.Context) (string, error) {
	if s == nil || s.Exchanger == nil {
		return "", errors.New("no service credential configured")
	}
	leeway := s.Leeway
	if leeway <= 0 {
		leeway = 30 * time.Second
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != nil && time.Until(s.tok.ExpiresAt) > leeway {
		return s.tok.AccessToken, nil
	}
	tok, err := s.Exchanger.ClientCredentials(ctx)
	if err != nil {
		return "", err
	}
	s.tok = tok
	return tok.AccessToken, nil
}
