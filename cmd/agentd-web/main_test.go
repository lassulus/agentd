package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// fakeIssuer serves OIDC discovery and a token endpoint that accepts code
// "c1" once (like a real IdP, a failed attempt uses it up), from a public
// client only, with the PKCE verifier matching the authorization request's
// challenge.
func fakeIssuer(t *testing.T, challenge *string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	var mu sync.Mutex
	used := map[string]bool{} // challenges whose code was tried
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": srv.URL, "jwks_uri": srv.URL + "/keys",
				"authorization_endpoint": srv.URL + "/auth", "token_endpoint": srv.URL + "/token",
			})
		case "/token":
			r.ParseForm()
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			_, _, basic := r.BasicAuth()
			mu.Lock()
			reused := used[*challenge]
			used[*challenge] = true
			mu.Unlock()
			if reused || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "c1" ||
				r.PostForm.Get("client_id") != "web" || basic || r.PostForm.Has("client_secret") ||
				base64.RawURLEncoding.EncodeToString(sum[:]) != *challenge {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok1", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type seen struct{ auth, cookie, uri, method string }

// fakeBackend records what reaches it; /v1/agents/a1/events streams one
// event and then holds the connection open.
func fakeBackend(t *testing.T, got chan<- seen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.URL.RequestURI(), r.Method}
		if r.URL.Path == "/v1/agents/a1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "id: 1\ndata: {\"seq\":1}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "[]")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, issuer string, backends ...string) (*server, *httptest.Server) {
	t.Helper()
	cfg := &Config{URL: "http://web.example"}
	cfg.OIDC.Issuer, cfg.OIDC.ClientID, cfg.OIDC.Scopes = issuer, "web", []string{"openid"}
	for i := 0; i < len(backends); i += 2 {
		cfg.Backends = append(cfg.Backends, struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}{backends[i], backends[i+1]})
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return s, srv
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func cookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login runs the authorization code flow like a browser would, with the
// IdP's redirect back simulated.
func login(t *testing.T, web string, challenge *string) *http.Cookie {
	t.Helper()
	resp, err := noRedirect.Get(web + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if resp.StatusCode != 302 || loc.Path != "/auth" || q.Get("client_id") != "web" ||
		q.Get("redirect_uri") != "http://web.example/callback" || q.Get("code_challenge_method") != "S256" ||
		q.Get("response_type") != "code" || q.Get("scope") != "openid" {
		t.Fatalf("login redirect: %d %s", resp.StatusCode, loc)
	}
	*challenge = q.Get("code_challenge")
	lc := cookie(resp, loginCookie)
	if lc == nil || !lc.HttpOnly || lc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("login cookie: %+v", lc)
	}

	req, _ := http.NewRequest("GET", web+"/callback?code=c1&state="+q.Get("state"), nil)
	req.AddCookie(lc)
	resp, err = noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := cookie(resp, sessionCookie)
	if resp.StatusCode != 302 || sc == nil || sc.Value == "" {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback: %d %s", resp.StatusCode, b)
	}
	return sc
}

func do(t *testing.T, method, url string, c *http.Cookie, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if c != nil {
		req.AddCookie(c)
	}
	for i := 0; i < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestLoginAndProxy(t *testing.T) {
	var challenge string
	iss := fakeIssuer(t, &challenge)
	got := make(chan seen, 10)
	b1, b2 := fakeBackend(t, got), fakeBackend(t, got)
	s, web := newTestServer(t, iss.URL, "one", b1.URL, "two", b2.URL)

	if r := do(t, "GET", web.URL+"/api/backends", nil); r.StatusCode != 401 {
		t.Fatalf("no session: %d", r.StatusCode)
	}
	if r := do(t, "GET", web.URL+"/api/b/one/v1/agents", nil); r.StatusCode != 401 {
		t.Fatalf("no session, proxy: %d", r.StatusCode)
	}
	if len(got) > 0 {
		t.Fatal("request without session reached the backend")
	}

	sc := login(t, web.URL, &challenge)

	r := do(t, "GET", web.URL+"/api/backends", sc)
	var names []string
	json.NewDecoder(r.Body).Decode(&names)
	if strings.Join(names, ",") != "one,two" {
		t.Fatalf("backends: %v", names)
	}

	r = do(t, "GET", web.URL+"/api/b/two/v1/agents?all=1", sc)
	if r.StatusCode != 200 {
		t.Fatalf("proxy: %d", r.StatusCode)
	}
	if g := <-got; g.auth != "Bearer tok1" || g.cookie != "" || g.uri != "/v1/agents?all=1" {
		t.Fatalf("backend got %+v", g)
	}

	if r := do(t, "GET", web.URL+"/api/b/nope/v1/agents", sc); r.StatusCode != 404 {
		t.Fatalf("unknown backend: %d", r.StatusCode)
	}

	// CSRF: a cross-site POST never reaches the backend.
	r = do(t, "POST", web.URL+"/api/b/one/v1/agents/a1/messages", sc, "Sec-Fetch-Site", "cross-site")
	if r.StatusCode != 403 || len(got) > 0 {
		t.Fatalf("cross-site POST: %d", r.StatusCode)
	}
	r = do(t, "POST", web.URL+"/api/b/one/v1/agents/a1/messages", sc, "Sec-Fetch-Site", "same-origin")
	if g := <-got; r.StatusCode != 200 || g.method != "POST" || g.uri != "/v1/agents/a1/messages" {
		t.Fatalf("same-origin POST: %d %+v", r.StatusCode, g)
	}

	// Server-sent events arrive while the backend keeps the stream open.
	r = do(t, "GET", web.URL+"/api/b/one/v1/agents/a1/events?since=0", sc)
	<-got
	line, err := bufio.NewReader(r.Body).ReadString('\n')
	if err != nil || line != "id: 1\n" {
		t.Fatalf("sse: %q %v", line, err)
	}

	// An expired access token ends the session.
	s.mu.Lock()
	s.sessions[sc.Value] = &oauth2.Token{AccessToken: "tok1", Expiry: time.Now().Add(-time.Minute)}
	s.mu.Unlock()
	if r := do(t, "GET", web.URL+"/api/b/one/v1/agents", sc); r.StatusCode != 401 || len(got) > 0 {
		t.Fatalf("expired: %d", r.StatusCode)
	}

	sc = login(t, web.URL, &challenge)
	do(t, "GET", web.URL+"/logout", sc)
	if r := do(t, "GET", web.URL+"/api/backends", sc); r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestCallbackRejects(t *testing.T) {
	var challenge string
	iss := fakeIssuer(t, &challenge)
	_, web := newTestServer(t, iss.URL)

	resp, _ := noRedirect.Get(web.URL + "/login")
	lc := cookie(resp, loginCookie)
	state := func() string { u, _ := url.Parse(resp.Header.Get("Location")); return u.Query().Get("state") }()
	challenge = func() string { u, _ := url.Parse(resp.Header.Get("Location")); return u.Query().Get("code_challenge") }()

	for name, tc := range map[string]struct {
		query  string
		cookie *http.Cookie
		code   int
	}{
		"no login cookie": {"code=c1&state=" + state, nil, 400},
		"wrong state":     {"code=c1&state=other", lc, 400},
		"wrong verifier":  {"code=c1&state=" + state, &http.Cookie{Name: loginCookie, Value: state + ".x"}, 502},
		"idp error":       {"error=access_denied", lc, 403},
	} {
		r := do(t, "GET", web.URL+"/callback?"+tc.query, tc.cookie)
		if r.StatusCode != tc.code || cookie(r, sessionCookie) != nil {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"url":"https://web.example/","oidc":{"issuer":"https://id.example","clientId":"web"},
		"backends":[{"name":"a","url":"http://127.0.0.1:1"}]}`), 0o600)
	c, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8792" || c.URL != "https://web.example" || c.OIDC.Scopes[0] != "openid" {
		t.Fatalf("%+v", c)
	}
	s, err := newServer(c)
	if err != nil || !s.secure {
		t.Fatalf("%v %v", s, err)
	}
	c.Backends = append(c.Backends, c.Backends[0])
	if _, err := newServer(c); err == nil {
		t.Fatal("duplicate backend name accepted")
	}
	os.WriteFile(p, []byte(`{"oidc":{"issuer":"https://id.example"}}`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("missing url/clientId accepted")
	}
}
