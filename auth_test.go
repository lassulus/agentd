package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fakeIssuer serves OIDC discovery and a JWKS, and signs access tokens.
func fakeIssuer(t *testing.T) (*httptest.Server, func(claims map[string]any) string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": srv.URL, "jwks_uri": srv.URL + "/keys",
				"authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(claims map[string]any) string {
		c := map[string]any{"iss": srv.URL, "aud": "agentd", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		for k, v := range claims {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		jws, err := signer.Sign(b)
		if err != nil {
			t.Fatal(err)
		}
		s, _ := jws.CompactSerialize()
		return s
	}
	return srv, sign
}

func TestOIDC(t *testing.T) {
	iss, sign := fakeIssuer(t)
	// The audience comes from a file, as for a client id the IdP generates.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "audience"), []byte("agentd\n"), 0o600)
	cfgPath := filepath.Join(dir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"stateDir":"`+dir+`","workspaceRoot":"`+dir+`",
		"auth":{"oidc":{"issuer":"`+iss.URL+`","audienceFile":"`+dir+`/audience"}},
		"users":["sub-lass","lass@example.com"]}`), 0o600)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthenticator(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		user   string
		code   int
	}{
		{"allowed sub", map[string]any{"sub": "sub-lass"}, "sub-lass", 0},
		{"verified email", map[string]any{"sub": "x1", "email": "lass@example.com", "email_verified": true}, "lass@example.com", 0},
		{"unverified email", map[string]any{"sub": "x2", "email": "lass@example.com", "email_verified": false}, "", 403},
		{"other user", map[string]any{"sub": "mallory"}, "", 403},
		{"wrong audience", map[string]any{"sub": "sub-lass", "aud": "other"}, "", 401},
		{"expired", map[string]any{"sub": "sub-lass", "exp": time.Now().Add(-time.Hour).Unix()}, "", 401},
		{"other issuer", map[string]any{"sub": "sub-lass", "iss": "https://evil.example"}, "", 401},
	} {
		r := httptest.NewRequest("GET", "/v1/agents", nil)
		r.Header.Set("Authorization", "Bearer "+sign(tc.claims))
		user, err := auth.user(r)
		code := 0
		if he, ok := err.(*httpError); ok {
			code = he.code
		} else if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if user != tc.user || code != tc.code {
			t.Errorf("%s: got user %q code %d (%v), want %q %d", tc.name, user, code, err, tc.user, tc.code)
		}
	}
}
