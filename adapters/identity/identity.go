// Package identity authenticates API callers: OAuth 2.1 bearer JWTs validated against an issuer's
// JWKS (production) or HS256 tokens signed with a shared secret (development only).
package identity

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"idp-host/internal/authorization"
)

// ErrNoCredentials means the request carried no bearer credential.
var ErrNoCredentials = errors.New("no bearer credential")

// ErrInvalid means a credential was presented and refused. The reason is never returned to clients.
var ErrInvalid = errors.New("invalid credential")

// Algorithms accepted for production tokens. HS* is accepted only by the development verifier.
var Algorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

// Bearer extracts the token of an "Authorization: Bearer <token>" header.
func Bearer(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return "", ErrNoCredentials
	}
	if len(values) > 1 {
		return "", ErrInvalid
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t") {
		return "", ErrInvalid
	}
	return token, nil
}

// Profile describes the tokens the HOST accepts.
type Profile struct {
	Issuer      string
	Audience    string
	JWKSURL     string
	TenantClaim string
	RolesClaim  string
}

// JWKS fetches and caches the signing keys of an issuer.
type JWKS struct {
	URL        string
	HTTP       *http.Client
	TTL        time.Duration // how long a fetched key set is trusted
	MinRefresh time.Duration // unknown kids trigger at most one fetch per MinRefresh

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time
	lastTry   time.Time
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := b64(k.N)
		e, err2 := b64(k.E)
		if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key (needs >= 2048-bit modulus)")
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
	case "EC":
		curves := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}
		curve, ok := curves[k.Crv]
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		if !ok || err1 != nil || err2 != nil {
			return nil, errors.New("bad EC key")
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return nil, errors.New("bad EC coordinates")
		}
		return ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

func (j *JWKS) fetch(ctx context.Context) (map[string]crypto.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, errors.New("JWKS is not a key set")
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := k.publicKey()
		if err != nil {
			continue // a key we cannot use is simply not trusted
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS has no usable signing keys")
	}
	return keys, nil
}

// Key returns the key for kid, refreshing the set when it is stale or kid is unknown.
func (j *JWKS) Key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	k, known := j.keys[kid]
	if known && now.Sub(j.fetchedAt) < j.TTL {
		return k, nil
	}
	if now.Sub(j.lastTry) >= j.MinRefresh || j.keys == nil {
		j.lastTry = now
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		keys, err := j.fetch(fctx)
		cancel()
		if err == nil {
			j.keys, j.fetchedAt = keys, now
			k, known = keys[kid]
		}
	}
	if !known {
		return nil, errors.New("unknown signing key")
	}
	return k, nil
}

// Verifier validates bearer tokens: signature, issuer, audience, exp, and the sub / tenant / roles
// claims.
type Verifier struct {
	profile Profile
	keys    *JWKS
	hmac    []byte
	leeway  time.Duration
}

// NewVerifier accepts asymmetric tokens signed by a key of the issuer's JWKS.
func NewVerifier(p Profile, keys *JWKS) *Verifier {
	return &Verifier{profile: p, keys: keys, leeway: 30 * time.Second}
}

// NewDevVerifier accepts HS256 tokens signed with secret. Development only.
func NewDevVerifier(p Profile, secret []byte) (*Verifier, error) {
	if len(secret) < 32 {
		return nil, errors.New("development secret must be at least 32 bytes")
	}
	return &Verifier{profile: p, hmac: secret, leeway: 30 * time.Second}, nil
}

// Dev reports whether this verifier accepts development tokens.
func (v *Verifier) Dev() bool { return v.hmac != nil }

// Verify checks token and returns its principal.
func (v *Verifier) Verify(ctx context.Context, token string) (authorization.Principal, error) {
	claims := jwt.MapClaims{}
	methods := Algorithms
	key := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.keys.Key(ctx, kid)
	}
	if v.hmac != nil {
		methods = []string{"HS256"}
		key = func(*jwt.Token) (any, error) { return v.hmac, nil }
	}
	if _, err := jwt.ParseWithClaims(token, claims, key, jwt.WithValidMethods(methods), jwt.WithIssuer(v.profile.Issuer),
		jwt.WithAudience(v.profile.Audience), jwt.WithExpirationRequired(), jwt.WithLeeway(v.leeway)); err != nil {
		return authorization.Principal{}, ErrInvalid
	}
	sub, _ := claims["sub"].(string)
	tenant, _ := claims[v.profile.TenantClaim].(string)
	rawRoles, ok := claims[v.profile.RolesClaim].([]any)
	if sub == "" || tenant == "" || !ok {
		return authorization.Principal{}, ErrInvalid
	}
	roles := make([]string, 0, len(rawRoles))
	for _, r := range rawRoles {
		s, ok := r.(string)
		if !ok {
			return authorization.Principal{}, ErrInvalid
		}
		roles = append(roles, s)
	}
	var expires time.Time
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		expires = exp.Time
	}
	azp, _ := claims["azp"].(string)
	var actor string
	if act, ok := claims["act"].(map[string]any); ok {
		if actor, _ = act["client_id"].(string); actor == "" {
			actor, _ = act["sub"].(string)
		}
	}
	method := "jwt"
	if v.hmac != nil {
		method = "dev"
	}
	return authorization.Principal{Subject: sub, Tenant: tenant, Roles: roles, Method: method, Expires: expires, Client: azp, Actor: actor}, nil
}

// DevToken signs a development token (idp-host dev-token, tests).
func DevToken(secret []byte, p Profile, subject, tenant string, roles []string, ttl time.Duration) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": p.Issuer, "aud": p.Audience, "sub": subject, p.TenantClaim: tenant, p.RolesClaim: roles,
		"iat": now.Unix(), "exp": now.Add(ttl).Unix(),
	}).SignedString(secret)
}
