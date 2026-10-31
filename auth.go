package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
)

// authenticator accepts two kinds of bearer tokens: API tokens (listed by
// their sha256 in the tokens file) and OAuth2 JWT access tokens from the
// configured OIDC issuer, for the users in Config.Users.
type authenticator struct {
	tokens map[string]string // hex sha256 of token -> user

	issuer   string
	audience string
	users    map[string]bool

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
}

func newAuthenticator(cfg *Config, tokensFile string) (*authenticator, error) {
	a := &authenticator{tokens: map[string]string{}, users: map[string]bool{}}
	if tokensFile != "" {
		f, err := os.Open(tokensFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		s := bufio.NewScanner(f)
		for n := 1; s.Scan(); n++ {
			line := strings.TrimSpace(s.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			f := strings.Fields(line)
			if len(f) != 2 || len(f[1]) != 64 {
				return nil, fmt.Errorf("%s:%d: want \"<user> <sha256 hex of token>\"", tokensFile, n)
			}
			a.tokens[strings.ToLower(f[1])] = f[0]
		}
		if err := s.Err(); err != nil {
			return nil, err
		}
	}
	if o := cfg.Auth.OIDC; o != nil {
		a.issuer, a.audience = o.Issuer, o.Audience
	}
	for _, u := range cfg.Users {
		a.users[u] = true
	}
	return a, nil
}

// oidcVerifier does the issuer discovery on first use and retries it on the
// next request if it failed, so agentd starts while the IdP is unreachable.
func (a *authenticator) oidcVerifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verifier != nil {
		return a.verifier, nil
	}
	p, err := oidc.NewProvider(context.WithoutCancel(ctx), a.issuer)
	if err != nil {
		return nil, err
	}
	a.verifier = p.Verifier(&oidc.Config{ClientID: a.audience})
	return a.verifier, nil
}

func (a *authenticator) user(r *http.Request) (string, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", errf(401, "missing bearer token")
	}
	sum := sha256.Sum256([]byte(tok))
	if u, ok := a.tokens[hex.EncodeToString(sum[:])]; ok {
		return u, nil
	}
	if a.issuer == "" || strings.Count(tok, ".") != 2 {
		return "", errf(401, "invalid token")
	}
	v, err := a.oidcVerifier(r.Context())
	if err != nil {
		return "", errf(503, "oidc discovery: %v", err)
	}
	t, err := v.Verify(r.Context(), tok)
	if err != nil {
		return "", errf(401, "invalid token: %v", err)
	}
	var c struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	}
	if err := t.Claims(&c); err != nil {
		return "", errf(401, "invalid token claims")
	}
	switch {
	case c.Email != "" && c.EmailVerified && a.users[c.Email]:
		return c.Email, nil
	case a.users[c.Sub]:
		return c.Sub, nil
	}
	return "", errf(403, "user %s is not allowed", c.Sub)
}
