// agentd-web is a web UI for one or more agentd backends. Users log in with
// OIDC (authorization code flow with PKCE, as a public client); the UI's API
// calls are forwarded to the backends with the user's access token as bearer
// token, so each backend decides with its own auth.oidc settings who may in.
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

//go:embed ui
var uiFiles embed.FS

type Config struct {
	Listen string `json:"listen"`
	// URL is where users reach this service; the OIDC redirect URI is
	// URL + "/callback".
	URL      string `json:"url"`
	Backends []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"backends"`
	OIDC struct {
		Issuer   string `json:"issuer"`
		ClientID string `json:"clientId"`
		// ClientIDFile holds the client id instead, for one that the IdP
		// generates at runtime.
		ClientIDFile string   `json:"clientIdFile"`
		Scopes       []string `json:"scopes"`
	} `json:"oidc"`
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8792"
	}
	if c.OIDC.ClientIDFile != "" {
		if c.OIDC.ClientID != "" {
			return nil, fmt.Errorf("%s: oidc: set clientId or clientIdFile, not both", path)
		}
		b, err := os.ReadFile(c.OIDC.ClientIDFile)
		if err != nil {
			return nil, fmt.Errorf("oidc.clientIdFile: %w", err)
		}
		c.OIDC.ClientID = strings.TrimSpace(string(b))
	}
	if c.URL == "" || c.OIDC.Issuer == "" || c.OIDC.ClientID == "" {
		return nil, fmt.Errorf("%s: url, oidc.issuer and oidc.clientId are required", path)
	}
	if len(c.OIDC.Scopes) == 0 {
		c.OIDC.Scopes = []string{oidc.ScopeOpenID}
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	return &c, nil
}

type server struct {
	cfg      *Config
	names    []string            // backend names in config order
	backends map[string]*url.URL // name -> base URL
	secure   bool                // cookies only over https

	mu       sync.Mutex
	oauth    *oauth2.Config
	sessions map[string]*oauth2.Token // session cookie -> the user's tokens
}

func newServer(cfg *Config) (*server, error) {
	s := &server{
		cfg:      cfg,
		backends: map[string]*url.URL{},
		secure:   strings.HasPrefix(cfg.URL, "https://"),
		sessions: map[string]*oauth2.Token{},
	}
	for _, b := range cfg.Backends {
		u, err := url.Parse(b.URL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("backend %q: invalid url %q", b.Name, b.URL)
		}
		if b.Name == "" || s.backends[b.Name] != nil {
			return nil, fmt.Errorf("backend names must be unique and non-empty: %q", b.Name)
		}
		s.backends[b.Name] = u
		s.names = append(s.names, b.Name)
	}
	return s, nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("/", http.FileServerFS(ui))
	mux.HandleFunc("GET /login", s.login)
	mux.HandleFunc("GET /callback", s.callback)
	mux.HandleFunc("GET /logout", s.logout)
	mux.HandleFunc("GET /api/backends", s.withToken(func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.names)
	}))
	mux.HandleFunc("/api/b/{backend}/", s.withToken(s.proxy))
	// The session cookie authenticates API calls, so reject cross-origin
	// state-changing requests (CSRF).
	return http.NewCrossOriginProtection().Handler(mux)
}

// oauthConfig does the issuer discovery on first use and retries it on the
// next login if it failed, so the service starts while the IdP is down.
func (s *server) oauthConfig(ctx context.Context) (*oauth2.Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauth != nil {
		return s.oauth, nil
	}
	p, err := oidc.NewProvider(context.WithoutCancel(ctx), s.cfg.OIDC.Issuer)
	if err != nil {
		return nil, err
	}
	ep := p.Endpoint()
	ep.AuthStyle = oauth2.AuthStyleInParams // public client: client_id in the body, no secret
	s.oauth = &oauth2.Config{
		ClientID:    s.cfg.OIDC.ClientID,
		Endpoint:    ep,
		RedirectURL: s.cfg.URL + "/callback",
		Scopes:      s.cfg.OIDC.Scopes,
	}
	return s.oauth, nil
}

const (
	sessionCookie = "agentd-web"
	loginCookie   = "agentd-web-login"
)

func (s *server) setCookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	oc, err := s.oauthConfig(r.Context())
	if err != nil {
		http.Error(w, "oidc discovery: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	state, verifier := rand.Text(), oauth2.GenerateVerifier()
	s.setCookie(w, loginCookie, state+"."+verifier, "/callback", 0)
	http.Redirect(w, r, oc.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (s *server) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "login failed: "+e+": "+q.Get("error_description"), http.StatusForbidden)
		return
	}
	var state, verifier string
	if c, err := r.Cookie(loginCookie); err == nil {
		state, verifier, _ = strings.Cut(c.Value, ".")
	}
	if state == "" || state != q.Get("state") {
		http.Error(w, "login state mismatch, try again: /login", http.StatusBadRequest)
		return
	}
	s.setCookie(w, loginCookie, "", "/callback", -1)
	oc, err := s.oauthConfig(r.Context())
	if err != nil {
		http.Error(w, "oidc discovery: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	tok, err := oc.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		http.Error(w, "token exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	id := rand.Text()
	s.mu.Lock()
	for k, t := range s.sessions {
		if !t.Valid() {
			delete(s.sessions, k)
		}
	}
	s.sessions[id] = tok
	s.mu.Unlock()
	s.setCookie(w, sessionCookie, id, "/", 0)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	s.setCookie(w, sessionCookie, "", "/", -1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `Logged out. <a href="/">Log in again</a>`)
}

// withToken passes the session's access token to f, or answers 401 so the
// UI starts a new login.
func (s *server) withToken(f func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var tok *oauth2.Token
		if c, err := r.Cookie(sessionCookie); err == nil {
			s.mu.Lock()
			tok = s.sessions[c.Value]
			if tok != nil && !tok.Valid() {
				delete(s.sessions, c.Value)
				tok = nil
			}
			s.mu.Unlock()
		}
		if tok == nil {
			http.Error(w, "not logged in", http.StatusUnauthorized)
			return
		}
		f(w, r, tok.AccessToken)
	}
}

// proxy forwards /api/b/<backend>/<path> to <backend url>/<path>.
func (s *server) proxy(w http.ResponseWriter, r *http.Request, token string) {
	name := r.PathValue("backend")
	target, ok := s.backends[name]
	if !ok {
		http.Error(w, "unknown backend "+name, http.StatusNotFound)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/api/b/"+name)
			pr.Out.URL.RawPath = ""
			pr.SetURL(target)
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Authorization", "Bearer "+token)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				slog.Warn("backend", "name", name, "err", err)
			}
			http.Error(w, "backend "+name+": "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func main() {
	configPath := flag.String("config", "/etc/agentd-web/config.json", "config file")
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	s, err := newServer(cfg)
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	slog.Info("listening", "addr", cfg.Listen, "backends", s.names)
	if err := http.ListenAndServe(cfg.Listen, s.routes()); err != nil {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
}
