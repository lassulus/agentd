package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The test binary doubles as a fake ACP agent when AGENTD_FAKE_AGENT=1.
func TestMain(m *testing.M) {
	if os.Getenv("AGENTD_FAKE_AGENT") == "1" {
		fakeAgent()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeAgent() {
	logf, _ := os.OpenFile(os.Getenv("AGENTD_FAKE_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	var mu sync.Mutex
	model := "fast"
	cancelled := make(chan struct{}, 1)
	options := func() any {
		return []any{map[string]any{
			"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": model,
			"options": []any{map[string]string{"value": "fast", "name": "Fast"}, map[string]string{"value": "smart", "name": "Smart"}},
		}}
	}
	var connP atomic.Pointer[rpcConn] // set after newRPCConn starts reading
	conn := func() *rpcConn { return connP.Load() }
	say := func(sid, text string) {
		_ = conn().Notify("session/update", map[string]any{"sessionId": sid, "update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": text}}})
	}
	handler := func(method string, params json.RawMessage) (any, *rpcError) {
		fmt.Fprintln(logf, method)
		var p struct {
			SessionID string `json:"sessionId"`
			ConfigID  string `json:"configId"`
			Value     string `json:"value"`
			Prompt    []struct {
				Text string `json:"text"`
			} `json:"prompt"`
		}
		_ = json.Unmarshal(params, &p)
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "initialize":
			return map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{
				"loadSession": true, "sessionCapabilities": map[string]any{"resume": map[string]any{}}}}, nil
		case "session/new":
			return map[string]any{"sessionId": "s1", "configOptions": options()}, nil
		case "session/resume":
			return map[string]any{"configOptions": options()}, nil
		case "session/set_config_option":
			if p.ConfigID != "model" || (p.Value != "fast" && p.Value != "smart") {
				return nil, &rpcError{Code: -32602, Message: "bad option"}
			}
			model = p.Value
			return map[string]any{"configOptions": options()}, nil
		case "session/cancel":
			cancelled <- struct{}{}
			return nil, nil
		case "session/prompt":
			text := p.Prompt[0].Text
			mu.Unlock()
			defer mu.Lock()
			switch {
			case strings.Contains(text, "perm"):
				var res struct {
					Outcome struct {
						Outcome  string `json:"outcome"`
						OptionID string `json:"optionId"`
					} `json:"outcome"`
				}
				_ = conn().Call(context.Background(), "session/request_permission", map[string]any{
					"sessionId": p.SessionID,
					"toolCall":  map[string]string{"toolCallId": "t1", "title": "rm -rf /tmp/x"},
					"options": []map[string]string{
						{"optionId": "allow", "name": "Yes", "kind": "allow_once"},
						{"optionId": "reject", "name": "No", "kind": "reject_once"}},
				}, &res)
				say(p.SessionID, "permission: "+res.Outcome.Outcome+" "+res.Outcome.OptionID)
			case strings.Contains(text, "cancelme"):
				<-cancelled
				return map[string]string{"stopReason": "cancelled"}, nil
			case strings.Contains(text, "slow"):
				time.Sleep(300 * time.Millisecond)
			}
			say(p.SessionID, "echo: "+text)
			say(p.SessionID, "!")
			return map[string]string{"stopReason": "end_turn"}, nil
		}
		return nil, &rpcError{Code: -32601, Message: "nope"}
	}
	c := newRPCConn(os.Stdin, os.Stdout, handler)
	connP.Store(c)
	<-c.done
}

type testEnv struct {
	t       *testing.T
	cfg     *Config
	srv     *httptest.Server
	m       *Manager
	fakeLog string
}

// newTestEnv starts agentd with the fake harness; extra adds config keys.
func newTestEnv(t *testing.T, stateDir string, extra ...map[string]any) *testEnv {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	fakeLog := filepath.Join(stateDir, "fake.log")
	cfgMap := map[string]any{
		"stateDir": stateDir, "workspaceRoot": root,
		"harnesses": map[string]any{"fake": map[string]any{
			"command": []string{os.Args[0]},
			"env":     map[string]string{"AGENTD_FAKE_AGENT": "1", "AGENTD_FAKE_LOG": fakeLog},
		}},
	}
	for _, x := range extra {
		maps.Copy(cfgMap, x)
	}
	cfgJSON, _ := json.Marshal(cfgMap)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfgPath, cfgJSON, 0o600)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("secret"))
	tokens := filepath.Join(t.TempDir(), "tokens")
	os.WriteFile(tokens, []byte("tester "+hex.EncodeToString(sum[:])+"\n"), 0o600)
	auth, err := newAuthenticator(cfg, tokens)
	if err != nil {
		t.Fatal(err)
	}
	m, err := newManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&server{m: m, auth: auth}).routes())
	t.Cleanup(func() { m.StopAll(); srv.Close() })
	return &testEnv{t: t, cfg: cfg, srv: srv, m: m, fakeLog: fakeLog}
}

func (e *testEnv) do(method, path string, body any, out any) int {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	req.Header.Set("Authorization", "Bearer secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil {
		if s, ok := out.(*string); ok {
			*s = string(b)
		} else if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return res.StatusCode
}

type historyRes struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
	State  string  `json:"state"`
}

// waitFor long-polls the history until an event matches.
func (e *testEnv) waitFor(id string, since int64, match func(Event) bool) Event {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var h historyRes
		e.do("GET", fmt.Sprintf("/v1/agents/%s/history?since=%d&wait=2", id, since), nil, &h)
		for _, ev := range h.Events {
			if match(ev) {
				return ev
			}
		}
		since = h.Next
	}
	e.t.Fatalf("agent %s: no matching event after seq %d", id, since)
	return Event{}
}

func turnEnd(e Event) bool { return e.Type == "turn_end" }

func (e *testEnv) lastSeq(id string) int64 {
	var v agentView
	e.do("GET", "/v1/agents/"+id, nil, &v)
	return v.LastSeq
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t, "")
	for _, tok := range []string{"", "Bearer wrong"} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/agents", nil)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("token %q: got %d, want 401", tok, res.StatusCode)
		}
	}
	if c := e.do("GET", "/v1/harnesses", nil, nil); c != 200 {
		t.Errorf("harnesses: %d", c)
	}
}

func TestCwdOutsideWorkspace(t *testing.T) {
	e := newTestEnv(t, "")
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(e.cfg.WorkspaceRoot, "link"))
	for _, cwd := range []string{"..", "/etc", outside, "link", "missing"} {
		var res map[string]string
		if c := e.do("POST", "/v1/agents", map[string]any{"harness": "fake", "cwd": cwd}, &res); c != 400 {
			t.Errorf("cwd %q: got %d %v, want 400", cwd, c, res)
		}
	}
}

func TestAgentLifecycle(t *testing.T) {
	stateDir := t.TempDir()
	e := newTestEnv(t, stateDir)

	var a agentView
	if c := e.do("POST", "/v1/agents", map[string]any{
		"harness": "fake", "cwd": "repo", "name": "t1",
		"config": map[string]any{"model": "smart"}, "prompt": "hello",
	}, &a); c != 201 {
		t.Fatalf("create: %d %+v", c, a)
	}
	if a.Model != "smart" || a.Cwd != filepath.Join(e.cfg.WorkspaceRoot, "repo") {
		t.Fatalf("create: %+v", a)
	}
	e.waitFor(a.ID, 0, turnEnd)
	var tr string
	e.do("GET", "/v1/agents/"+a.ID+"/transcript", nil, &tr)
	if !strings.Contains(tr, "**tester:** hello") || !strings.Contains(tr, "**agent:** echo: hello!") {
		t.Fatalf("transcript:\n%s", tr)
	}

	// Permission: held until answered, first answer wins.
	seq := e.lastSeq(a.ID)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "perm please"}, nil)
	req := e.waitFor(a.ID, seq, func(ev Event) bool { return ev.Type == "permission_request" })
	var pend []Event
	e.do("GET", "/v1/agents/"+a.ID+"/permissions", nil, &pend)
	if len(pend) != 1 || pend[0].ID != req.ID || len(pend[0].Options) != 2 {
		t.Fatalf("pending: %+v", pend)
	}
	var v agentView
	e.do("GET", "/v1/agents/"+a.ID, nil, &v)
	if v.State != "waiting" || v.PendingPermissions != 1 {
		t.Fatalf("state while waiting: %+v", v)
	}
	if c := e.do("POST", "/v1/agents/"+a.ID+"/permissions/"+req.ID, map[string]string{"option": "maybe"}, nil); c != 400 {
		t.Errorf("bad option: %d", c)
	}
	if c := e.do("POST", "/v1/agents/"+a.ID+"/permissions/"+req.ID, map[string]string{"option": "allow"}, nil); c != 204 {
		t.Fatalf("answer: %d", c)
	}
	if c := e.do("POST", "/v1/agents/"+a.ID+"/permissions/"+req.ID, map[string]string{"option": "reject"}, nil); c != 409 {
		t.Errorf("second answer: %d", c)
	}
	e.waitFor(a.ID, seq, turnEnd)
	e.do("GET", "/v1/agents/"+a.ID+"/transcript?last=3", nil, &tr)
	if !strings.Contains(tr, "permission: selected allow") || !strings.Contains(tr, "tester answered "+req.ID+": allow") {
		t.Fatalf("transcript after permission:\n%s", tr)
	}

	// Config.
	var cfg []configView
	if c := e.do("PATCH", "/v1/agents/"+a.ID+"/config", map[string]string{"model": "fast"}, &cfg); c != 200 {
		t.Fatalf("patch config: %d", c)
	}
	if len(cfg) != 1 || string(cfg[0].Value) != `"fast"` || len(cfg[0].Options) != 2 {
		t.Fatalf("config: %+v", cfg)
	}
	if c := e.do("PATCH", "/v1/agents/"+a.ID+"/config", map[string]string{"colour": "red"}, nil); c != 400 {
		t.Errorf("unknown option: %d", c)
	}

	// A message sent during a turn is queued and runs afterwards.
	seq = e.lastSeq(a.ID)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "slow one"}, nil)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "second"}, nil)
	e.do("GET", "/v1/agents/"+a.ID, nil, &v)
	if v.Queued != 1 {
		t.Errorf("queued = %d, want 1", v.Queued)
	}
	first := e.waitFor(a.ID, seq, turnEnd)
	e.waitFor(a.ID, first.Seq, turnEnd)
	e.do("GET", "/v1/agents/"+a.ID+"/transcript?last=4", nil, &tr)
	if i, j := strings.Index(tr, "echo: slow one"), strings.Index(tr, "echo: second"); i < 0 || j < i {
		t.Fatalf("queue order:\n%s", tr)
	}

	// Cancel.
	seq = e.lastSeq(a.ID)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "cancelme"}, nil)
	e.waitFor(a.ID, seq, func(ev Event) bool { return ev.Type == "state" && ev.State == "working" })
	if c := e.do("POST", "/v1/agents/"+a.ID+"/cancel", nil, nil); c != 204 {
		t.Fatalf("cancel: %d", c)
	}
	if ev := e.waitFor(a.ID, seq, turnEnd); ev.StopReason != "cancelled" {
		t.Fatalf("cancel turn_end: %+v", ev)
	}

	// Stop keeps the history; the next message resumes the session.
	if c := e.do("DELETE", "/v1/agents/"+a.ID, nil, nil); c != 204 {
		t.Fatalf("stop: %d", c)
	}
	var list []agentView
	e.do("GET", "/v1/agents", nil, &list)
	if len(list) != 0 {
		t.Errorf("open agents after stop: %+v", list)
	}
	e.do("GET", "/v1/agents?all=1", nil, &list)
	if len(list) != 1 || list[0].State != "stopped" {
		t.Fatalf("all agents: %+v", list)
	}
	seq = e.lastSeq(a.ID)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "after stop"}, nil)
	e.waitFor(a.ID, seq, turnEnd)
	if log, _ := os.ReadFile(e.fakeLog); strings.Count(string(log), "session/resume") != 1 || strings.Count(string(log), "session/new") != 1 {
		t.Fatalf("fake agent calls:\n%s", log)
	}

	// A new agentd process on the same state dir keeps agents and history.
	e.m.StopAll()
	e2 := newTestEnv(t, stateDir)
	var v2 agentView
	if c := e2.do("GET", "/v1/agents/"+a.ID, nil, &v2); c != 200 || v2.State != "stopped" || v2.LastSeq < seq {
		t.Fatalf("after restart: %d %+v", c, v2)
	}
	seq = v2.LastSeq
	e2.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "after restart"}, nil)
	e2.waitFor(a.ID, seq, turnEnd)
	e2.do("GET", "/v1/agents/"+a.ID+"/transcript?last=2", nil, &tr)
	if !strings.Contains(tr, "echo: after restart!") {
		t.Fatalf("transcript after restart:\n%s", tr)
	}

	// SSE replays from Last-Event-ID.
	req2, _ := http.NewRequest("GET", e2.srv.URL+"/v1/agents/"+a.ID+"/events", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	req2.Header.Set("Last-Event-ID", "1")
	res, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Scan()
	if sc.Text() != "id: 2" {
		t.Errorf("first SSE line %q, want id: 2", sc.Text())
	}
	res.Body.Close()

	if c := e2.do("DELETE", "/v1/agents/"+a.ID+"?purge=1", nil, nil); c != 204 {
		t.Fatalf("purge: %d", c)
	}
	if c := e2.do("GET", "/v1/agents/"+a.ID, nil, nil); c != 404 {
		t.Errorf("after purge: %d", c)
	}
}
