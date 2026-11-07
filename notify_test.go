package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookServer records the payloads it receives and checks their signature
// (none expected when secret is "").
func hookServer(t *testing.T, secret string) (*httptest.Server, chan notifyPayload) {
	got := make(chan notifyPayload, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		want := ""
		if secret != "" {
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			want = "sha256=" + hex.EncodeToString(mac.Sum(nil))
		}
		if r.Header.Get("X-Hub-Signature-256") != want {
			t.Errorf("signature %q, want %q", r.Header.Get("X-Hub-Signature-256"), want)
		}
		var p notifyPayload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("payload: %v: %s", err, body)
		}
		got <- p
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func nextPayload(t *testing.T, got chan notifyPayload) notifyPayload {
	t.Helper()
	select {
	case p := <-got:
		return p
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
		return notifyPayload{}
	}
}

func noPayload(t *testing.T, got chan notifyPayload) {
	t.Helper()
	select {
	case p := <-got:
		t.Fatalf("unexpected notification: %+v", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestNotify(t *testing.T) {
	perms, permsGot := hookServer(t, "hooksecret")
	turns, turnsGot := hookServer(t, "")
	secretFile := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secretFile, []byte("hooksecret\n"), 0o600)

	e := newTestEnv(t, "", map[string]any{"notify": []any{
		map[string]any{"url": perms.URL, "secretFile": secretFile, "events": []string{"permission_request"}},
		map[string]any{"url": turns.URL, "events": []string{"turn_end"}},
	}})

	var a agentView
	e.do("POST", "/v1/agents", map[string]any{"harness": "fake", "cwd": "repo", "prompt": "perm please"}, &a)
	p := nextPayload(t, permsGot)
	if p.Type != "permission_request" || p.Agent.ID != a.ID || p.Agent.State != "waiting" ||
		p.Text != "> permission "+p.Event.ID+": rm -rf /tmp/x [allow, reject]" {
		t.Fatalf("permission notification: %+v", p)
	}
	e.do("POST", "/v1/agents/"+a.ID+"/permissions/"+p.Event.ID, map[string]string{"option": "allow"}, nil)
	p = nextPayload(t, turnsGot)
	if p.Type != "turn_end" || !strings.HasSuffix(p.Text, "echo: perm please!") {
		t.Fatalf("turn_end notification: %+v", p)
	}
	// Each webhook gets only its own event types (and none of acp, state, ...).
	noPayload(t, permsGot)
	noPayload(t, turnsGot)
}
