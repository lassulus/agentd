package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Event is one entry of an agent's history. `acp` events carry the agent's
// session/update payload unchanged; the other types are agentd's own.
type Event struct {
	Seq        int64           `json:"seq"`
	TS         time.Time       `json:"ts"`
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	By         string          `json:"by,omitempty"`
	Update     json.RawMessage `json:"update,omitempty"`
	ID         string          `json:"id,omitempty"`
	ToolCall   json.RawMessage `json:"tool_call,omitempty"`
	Options    []permOption    `json:"options,omitempty"`
	Request    json.RawMessage `json:"request,omitempty"`
	Option     string          `json:"option,omitempty"`
	Answer     json.RawMessage `json:"answer,omitempty"`
	Values     json.RawMessage `json:"values,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      json.RawMessage `json:"usage,omitempty"`
	State      string          `json:"state,omitempty"`
	Error      string          `json:"error,omitempty"`
}

type permOption struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// Meta is what agentd keeps about an agent besides its history.
type Meta struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Harness       string          `json:"harness"`
	Cwd           string          `json:"cwd"`
	Owner         string          `json:"owner"`
	Created       time.Time       `json:"created"`
	Updated       time.Time       `json:"updated"`
	SessionID     string          `json:"session_id"`
	State         string          `json:"state"`
	Error         string          `json:"error,omitempty"`
	ConfigOptions json.RawMessage `json:"config_options,omitempty"`
	Models        json.RawMessage `json:"models,omitempty"`
}

type pendingReq struct {
	id      string
	kind    string // "permission" or "question"
	options []permOption
	reply   chan json.RawMessage
}

type queuedMsg struct{ text string }

type Agent struct {
	m   *Manager
	dir string

	startMu sync.Mutex // serialises spawns

	mu      sync.Mutex
	meta    Meta
	events  []Event // events[i].Seq == i+1
	logf    *os.File
	changed chan struct{} // closed and replaced on every new event

	cmd       *exec.Cmd
	conn      *rpcConn
	exited    chan struct{}
	starting  bool
	stopping  bool
	busy      bool
	loading   bool // suppress the history replay of session/load
	canResume bool
	canLoad   bool
	queue     []queuedMsg
	pending   map[string]*pendingReq
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func errf(code int, format string, args ...any) error {
	return &httpError{code: code, msg: fmt.Sprintf(format, args...)}
}

func randomID(prefix string) string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return prefix + strings.ToLower(base32.StdEncoding.EncodeToString(b))
}

// --- history ---------------------------------------------------------------

func (a *Agent) appendLocked(e Event) {
	e.Seq = int64(len(a.events)) + 1
	e.TS = time.Now().UTC()
	if b, err := json.Marshal(e); err == nil {
		if _, err := a.logf.Write(append(b, '\n')); err != nil {
			slog.Error("write history", "agent", a.meta.ID, "err", err)
		}
	}
	a.events = append(a.events, e)
	a.meta.Updated = e.TS
	close(a.changed)
	a.changed = make(chan struct{})
	a.notifyLocked(e)
}

// eventsAfter returns up to limit events with seq > since; since < 0 means
// the last limit events.
func (a *Agent) eventsAfter(since int64, limit int) []Event {
	n := int64(len(a.events))
	start := since
	if since < 0 {
		start = max(n-int64(limit), 0)
	}
	if start > n {
		start = n
	}
	end := min(start+int64(limit), n)
	return append([]Event(nil), a.events[start:end]...)
}

func (a *Agent) saveMetaLocked() {
	b, err := json.MarshalIndent(a.meta, "", "  ")
	if err == nil {
		tmp := filepath.Join(a.dir, "meta.json.tmp")
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, filepath.Join(a.dir, "meta.json"))
		}
	}
	if err != nil {
		slog.Error("save meta", "agent", a.meta.ID, "err", err)
	}
}

func (a *Agent) stateLocked() string {
	switch {
	case a.starting:
		return "starting"
	case a.cmd == nil && a.meta.Error != "":
		return "error"
	case a.cmd == nil:
		return "stopped"
	case len(a.pending) > 0:
		return "waiting"
	case a.busy:
		return "working"
	default:
		return "idle"
	}
}

// syncStateLocked records a state change in the history and in meta.json.
func (a *Agent) syncStateLocked() {
	s := a.stateLocked()
	if s == a.meta.State {
		return
	}
	a.meta.State = s
	e := Event{Type: "state", State: s}
	if s == "error" {
		e.Error = a.meta.Error
	}
	a.appendLocked(e)
	a.saveMetaLocked()
}

// --- process ---------------------------------------------------------------

func (a *Agent) ensureRunning(ctx context.Context) error {
	a.startMu.Lock()
	defer a.startMu.Unlock()
	a.mu.Lock()
	if a.cmd != nil {
		a.mu.Unlock()
		return nil
	}
	a.starting = true
	a.meta.Error = ""
	a.syncStateLocked()
	a.mu.Unlock()

	err := a.spawn(ctx)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.starting = false
	if err != nil {
		a.meta.Error = err.Error()
	}
	a.syncStateLocked()
	return err
}

func (a *Agent) spawn(ctx context.Context) error {
	h, ok := a.m.cfg.Harnesses[a.meta.Harness]
	if !ok {
		return fmt.Errorf("harness %q is not configured", a.meta.Harness)
	}
	cmd := exec.Command(h.Command[0], h.Command[1:]...)
	cmd.Dir = a.meta.Cwd
	cmd.Env = os.Environ()
	for k, v := range h.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		s := bufio.NewScanner(stderr)
		s.Buffer(nil, 1<<20)
		for s.Scan() {
			slog.Info("agent stderr", "agent", a.meta.ID, "line", s.Text())
		}
	}()
	conn := newRPCConn(stdout, stdin, a.handle)
	exited := make(chan struct{})

	a.mu.Lock()
	a.cmd, a.conn, a.exited = cmd, conn, exited
	a.mu.Unlock()
	go a.wait(cmd, exited)

	if err := a.openSession(ctx, conn); err != nil {
		a.kill(cmd, exited)
		return err
	}
	return nil
}

func (a *Agent) openSession(ctx context.Context, conn *rpcConn) error {
	var init struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				Resume json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
		} `json:"agentCapabilities"`
	}
	err := conn.Call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":          map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal":    false,
			"elicitation": map[string]any{"form": map[string]any{}},
		},
		"clientInfo": map[string]string{"name": "agentd", "version": version},
	}, &init)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	a.mu.Lock()
	a.canResume = len(init.AgentCapabilities.SessionCapabilities.Resume) > 0 && string(init.AgentCapabilities.SessionCapabilities.Resume) != "null"
	a.canLoad = init.AgentCapabilities.LoadSession
	sid, cwd := a.meta.SessionID, a.meta.Cwd
	a.mu.Unlock()

	var res struct {
		SessionID     string          `json:"sessionId"`
		ConfigOptions json.RawMessage `json:"configOptions"`
		Models        json.RawMessage `json:"models"`
	}
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	switch {
	case sid == "":
		err = conn.Call(ctx, "session/new", params, &res)
	case a.canResume:
		params["sessionId"] = sid
		err = conn.Call(ctx, "session/resume", params, &res)
	case a.canLoad:
		params["sessionId"] = sid
		a.mu.Lock()
		a.loading = true
		a.mu.Unlock()
		err = conn.Call(ctx, "session/load", params, &res)
		a.mu.Lock()
		a.loading = false
		a.mu.Unlock()
	default:
		return errors.New("the harness can neither resume nor load sessions")
	}
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if res.SessionID != "" {
		a.meta.SessionID = res.SessionID
	}
	if len(res.ConfigOptions) > 0 {
		a.meta.ConfigOptions = res.ConfigOptions
	}
	if len(res.Models) > 0 && string(res.Models) != "null" {
		a.meta.Models = res.Models
	}
	a.saveMetaLocked()
	return nil
}

func (a *Agent) wait(cmd *exec.Cmd, exited chan struct{}) {
	err := cmd.Wait()
	close(exited)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cmd != cmd {
		return
	}
	a.cmd, a.conn = nil, nil
	a.busy = false
	a.queue = nil
	a.pending = map[string]*pendingReq{}
	if !a.stopping && !a.starting {
		msg := "agent exited"
		if err != nil {
			msg = "agent exited: " + err.Error()
		}
		a.meta.Error = msg
	}
	a.stopping = false
	a.syncStateLocked()
}

func (a *Agent) kill(cmd *exec.Cmd, exited chan struct{}) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
}

// Stop ends the agent's process; the history stays.
func (a *Agent) Stop() {
	a.mu.Lock()
	cmd, exited := a.cmd, a.exited
	a.queue = nil
	if cmd != nil {
		a.stopping = true
	}
	a.meta.Error = ""
	a.syncStateLocked()
	a.mu.Unlock()
	if cmd != nil {
		a.kill(cmd, exited)
	}
}

// --- requests from the agent -----------------------------------------------

func (a *Agent) handle(method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "session/update":
		a.onUpdate(params)
		return nil, nil
	case "session/request_permission":
		return a.onPermission(params), nil
	case "elicitation/create":
		return a.onQuestion(params), nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + method}
}

func (a *Agent) onUpdate(params json.RawMessage) {
	var p struct {
		Update json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Update) == 0 {
		return
	}
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	_ = json.Unmarshal(p.Update, &u)
	a.mu.Lock()
	defer a.mu.Unlock()
	if u.SessionUpdate == "config_option_update" && len(u.ConfigOptions) > 0 {
		a.meta.ConfigOptions = u.ConfigOptions
		a.saveMetaLocked()
	}
	if a.loading {
		return
	}
	a.appendLocked(Event{Type: "acp", Update: p.Update})
}

var permissionCancelled = json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`)
var questionCancelled = json.RawMessage(`{"action":"cancel"}`)

// waitPending registers a request and blocks until a user answers it or the
// agent connection goes away. There is no timeout.
func (a *Agent) waitPending(pr *pendingReq, ev Event, cancelled json.RawMessage) json.RawMessage {
	a.mu.Lock()
	conn := a.conn
	if conn == nil {
		a.mu.Unlock()
		return cancelled
	}
	a.pending[pr.id] = pr
	a.appendLocked(ev)
	a.syncStateLocked()
	a.mu.Unlock()
	select {
	case r := <-pr.reply:
		return r
	case <-conn.done:
		return cancelled
	}
}

func (a *Agent) onPermission(params json.RawMessage) json.RawMessage {
	var p struct {
		ToolCall json.RawMessage `json:"toolCall"`
		Options  []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(params, &p)
	pr := &pendingReq{id: randomID("p_"), kind: "permission", reply: make(chan json.RawMessage, 1)}
	for _, o := range p.Options {
		pr.options = append(pr.options, permOption{ID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	ev := Event{Type: "permission_request", ID: pr.id, ToolCall: p.ToolCall, Options: pr.options}
	return a.waitPending(pr, ev, permissionCancelled)
}

func (a *Agent) onQuestion(params json.RawMessage) json.RawMessage {
	pr := &pendingReq{id: randomID("q_"), kind: "question", reply: make(chan json.RawMessage, 1)}
	ev := Event{Type: "question", ID: pr.id, Request: params}
	return a.waitPending(pr, ev, questionCancelled)
}

// --- actions from users ----------------------------------------------------

// Send records a user message and starts a turn, or queues it while a turn
// runs. A stopped agent is started (and its session resumed) first.
func (a *Agent) Send(ctx context.Context, text, by string) error {
	if err := a.ensureRunning(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == nil {
		return errf(409, "agent stopped")
	}
	a.appendLocked(Event{Type: "user_message", Text: text, By: by})
	if a.busy {
		a.queue = append(a.queue, queuedMsg{text: text})
		return nil
	}
	a.startTurnLocked(text)
	return nil
}

func (a *Agent) startTurnLocked(text string) {
	a.busy = true
	a.syncStateLocked()
	conn, sid := a.conn, a.meta.SessionID
	go func() {
		var res struct {
			StopReason string          `json:"stopReason"`
			Usage      json.RawMessage `json:"usage"`
		}
		err := conn.Call(context.Background(), "session/prompt", map[string]any{
			"sessionId": sid,
			"prompt":    []map[string]string{{"type": "text", "text": text}},
		}, &res)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.conn != conn {
			return
		}
		ev := Event{Type: "turn_end", StopReason: res.StopReason, Usage: res.Usage}
		if err != nil {
			ev.Error = err.Error()
		}
		a.appendLocked(ev)
		a.busy = false
		if len(a.queue) > 0 {
			next := a.queue[0]
			a.queue = a.queue[1:]
			a.startTurnLocked(next.text)
			return
		}
		a.syncStateLocked()
	}()
}

// Cancel ends the running turn, drops queued messages and cancels pending
// permission requests, as ACP's session/cancel requires.
func (a *Agent) Cancel(by string) error {
	a.mu.Lock()
	conn, sid := a.conn, a.meta.SessionID
	if conn == nil {
		a.mu.Unlock()
		return errf(409, "agent is not running")
	}
	a.queue = nil
	for id, pr := range a.pending {
		if pr.kind == "permission" {
			pr.reply <- permissionCancelled
		} else {
			pr.reply <- questionCancelled
		}
		delete(a.pending, id)
		a.appendLocked(Event{Type: "permission_resolved", ID: id, By: by, Error: "cancelled"})
	}
	a.syncStateLocked()
	a.mu.Unlock()
	return conn.Notify("session/cancel", map[string]string{"sessionId": sid})
}

type answerBody struct {
	Option  string          `json:"option"`
	Answer  json.RawMessage `json:"answer"`
	Decline bool            `json:"decline"`
}

// Answer resolves a pending permission or question. The first answer wins.
func (a *Agent) Answer(id string, body answerBody, by string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	pr, ok := a.pending[id]
	if !ok {
		return errf(409, "request %s is not pending", id)
	}
	ev := Event{Type: "permission_resolved", ID: id, By: by}
	switch pr.kind {
	case "permission":
		found := false
		for _, o := range pr.options {
			found = found || o.ID == body.Option
		}
		if !found {
			return errf(400, "option %q is not one of the request's options", body.Option)
		}
		r, _ := json.Marshal(map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": body.Option}})
		pr.reply <- r
		ev.Option = body.Option
	case "question":
		switch {
		case body.Decline:
			pr.reply <- json.RawMessage(`{"action":"decline"}`)
			ev.Option = "decline"
		case len(body.Answer) > 0:
			r, _ := json.Marshal(map[string]any{"action": "accept", "content": body.Answer})
			pr.reply <- r
			ev.Option, ev.Answer = "accept", body.Answer
		default:
			return errf(400, "a question needs \"answer\" or \"decline\"")
		}
	}
	delete(a.pending, id)
	a.appendLocked(ev)
	a.syncStateLocked()
	return nil
}

func (a *Agent) Pending() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Event
	for _, e := range a.events {
		if _, ok := a.pending[e.ID]; ok && (e.Type == "permission_request" || e.Type == "question") {
			out = append(out, e)
		}
	}
	return out
}

// SetConfig changes session config options (model, effort, mode, ...). The
// option ids and values come from the harness. Harnesses that only know the
// older session/set_model get "model" mapped onto it.
func (a *Agent) SetConfig(ctx context.Context, values map[string]json.RawMessage, by string) error {
	if err := a.ensureRunning(ctx); err != nil {
		return err
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := a.setOne(ctx, k, values[k]); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(values)
	a.mu.Lock()
	a.appendLocked(Event{Type: "config_changed", Values: b, By: by})
	a.mu.Unlock()
	return nil
}

func (a *Agent) setOne(ctx context.Context, key string, value json.RawMessage) error {
	a.mu.Lock()
	conn, sid := a.conn, a.meta.SessionID
	opts := parseConfigOptions(a.meta.ConfigOptions)
	models := a.meta.Models
	a.mu.Unlock()
	if conn == nil {
		return errf(409, "agent stopped")
	}
	var isBool bool
	if json.Unmarshal(value, &isBool) == nil {
		isBool = true
	}
	for _, o := range opts {
		if o.ID != key {
			continue
		}
		params := map[string]any{"sessionId": sid, "configId": key, "value": value}
		if isBool {
			params["type"] = "boolean"
		}
		var res struct {
			ConfigOptions json.RawMessage `json:"configOptions"`
		}
		if err := conn.Call(ctx, "session/set_config_option", params, &res); err != nil {
			return errf(400, "%s: %v", key, err)
		}
		if len(res.ConfigOptions) > 0 {
			a.mu.Lock()
			a.meta.ConfigOptions = res.ConfigOptions
			a.saveMetaLocked()
			a.mu.Unlock()
		}
		return nil
	}
	if key == "model" && len(models) > 0 {
		var id string
		if json.Unmarshal(value, &id) != nil {
			return errf(400, "model must be a string")
		}
		if err := conn.Call(ctx, "session/set_model", map[string]any{"sessionId": sid, "modelId": id}, nil); err != nil {
			return errf(400, "model: %v", err)
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(models, &m) == nil {
			m["currentModelId"], _ = json.Marshal(id)
			a.mu.Lock()
			a.meta.Models, _ = json.Marshal(m)
			a.saveMetaLocked()
			a.mu.Unlock()
		}
		return nil
	}
	return errf(400, "unknown config option %q", key)
}

// --- views -----------------------------------------------------------------

type configView struct {
	ID       string          `json:"id"`
	Name     string          `json:"name,omitempty"`
	Category string          `json:"category,omitempty"`
	Type     string          `json:"type,omitempty"`
	Value    json.RawMessage `json:"value"`
	Options  []string        `json:"options,omitempty"`
}

func parseConfigOptions(raw json.RawMessage) []configView {
	var opts []struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Category     string          `json:"category"`
		Type         string          `json:"type"`
		CurrentValue json.RawMessage `json:"currentValue"`
		Options      []struct {
			Value   string `json:"value"`
			Options []struct {
				Value string `json:"value"`
			} `json:"options"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &opts)
	out := make([]configView, 0, len(opts))
	for _, o := range opts {
		v := configView{ID: o.ID, Name: o.Name, Category: o.Category, Type: o.Type, Value: o.CurrentValue}
		for _, x := range o.Options {
			if x.Value != "" {
				v.Options = append(v.Options, x.Value)
			}
			for _, y := range x.Options {
				v.Options = append(v.Options, y.Value)
			}
		}
		out = append(out, v)
	}
	return out
}

func (a *Agent) configLocked() []configView {
	opts := parseConfigOptions(a.meta.ConfigOptions)
	for _, o := range opts {
		if o.ID == "model" {
			return opts
		}
	}
	var m struct {
		Current   string `json:"currentModelId"`
		Available []struct {
			ModelID string `json:"modelId"`
		} `json:"availableModels"`
	}
	if json.Unmarshal(a.meta.Models, &m) == nil && m.Current != "" {
		v := configView{ID: "model", Name: "Model", Category: "model", Type: "select"}
		v.Value, _ = json.Marshal(m.Current)
		for _, x := range m.Available {
			v.Options = append(v.Options, x.ModelID)
		}
		opts = append(opts, v)
	}
	return opts
}

type agentView struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Harness            string       `json:"harness"`
	Cwd                string       `json:"cwd"`
	Owner              string       `json:"owner"`
	State              string       `json:"state"`
	Error              string       `json:"error,omitempty"`
	Model              string       `json:"model,omitempty"`
	SessionID          string       `json:"session_id,omitempty"`
	Created            time.Time    `json:"created"`
	Updated            time.Time    `json:"updated"`
	PendingPermissions int          `json:"pending_permissions"`
	Queued             int          `json:"queued"`
	LastSeq            int64        `json:"last_seq"`
	Config             []configView `json:"config,omitempty"`
}

func (a *Agent) View(withConfig bool) agentView {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.viewLocked(withConfig)
}

func (a *Agent) viewLocked(withConfig bool) agentView {
	cfg := a.configLocked()
	v := agentView{
		ID: a.meta.ID, Name: a.meta.Name, Harness: a.meta.Harness, Cwd: a.meta.Cwd, Owner: a.meta.Owner,
		State: a.stateLocked(), Error: a.meta.Error, SessionID: a.meta.SessionID,
		Created: a.meta.Created, Updated: a.meta.Updated,
		PendingPermissions: len(a.pending), Queued: len(a.queue), LastSeq: int64(len(a.events)),
	}
	for _, o := range cfg {
		var s string
		if o.Category == "model" && json.Unmarshal(o.Value, &s) == nil {
			v.Model = s
		}
	}
	if withConfig {
		v.Config = cfg
	}
	return v
}

// --- manager ---------------------------------------------------------------

type Manager struct {
	cfg *Config
	dir string

	mu     sync.Mutex
	agents map[string]*Agent
}

func newManager(cfg *Config) (*Manager, error) {
	m := &Manager{cfg: cfg, dir: filepath.Join(cfg.StateDir, "agents"), agents: map[string]*Agent{}}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a, err := m.loadAgent(filepath.Join(m.dir, e.Name()))
		if err != nil {
			slog.Error("skipping agent", "dir", e.Name(), "err", err)
			continue
		}
		m.agents[a.meta.ID] = a
	}
	return m, nil
}

func (m *Manager) newAgent(dir string, meta Meta) (*Agent, error) {
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Agent{m: m, dir: dir, meta: meta, logf: f, changed: make(chan struct{}), pending: map[string]*pendingReq{}}, nil
}

func (m *Manager) loadAgent(dir string) (*Agent, error) {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, err
	}
	var events []Event
	if f, err := os.Open(filepath.Join(dir, "events.jsonl")); err == nil {
		s := bufio.NewScanner(f)
		s.Buffer(nil, 64<<20)
		for s.Scan() {
			var e Event
			if json.Unmarshal(s.Bytes(), &e) == nil {
				e.Seq = int64(len(events)) + 1
				events = append(events, e)
			}
		}
		f.Close()
	}
	a, err := m.newAgent(dir, meta)
	if err != nil {
		return nil, err
	}
	a.events = events
	a.mu.Lock()
	a.syncStateLocked() // a running agent from before a restart is stopped now
	a.mu.Unlock()
	return a, nil
}

// resolveCwd returns cwd (absolute or relative to the workspace root) with
// symlinks resolved, and refuses anything outside the workspace root.
func (m *Manager) resolveCwd(cwd string) (string, error) {
	root := m.cfg.WorkspaceRoot
	if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	p, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", errf(400, "cwd: %v", err)
	}
	if st, err := os.Stat(p); err != nil || !st.IsDir() {
		return "", errf(400, "cwd %s is not a directory", p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", errf(400, "cwd %s is outside the workspace root %s", p, root)
	}
	return p, nil
}

type createBody struct {
	Harness string                     `json:"harness"`
	Cwd     string                     `json:"cwd"`
	Name    string                     `json:"name"`
	Config  map[string]json.RawMessage `json:"config"`
	Prompt  string                     `json:"prompt"`
}

func (m *Manager) Create(ctx context.Context, body createBody, owner string) (*Agent, error) {
	if _, ok := m.cfg.Harnesses[body.Harness]; !ok {
		return nil, errf(400, "unknown harness %q", body.Harness)
	}
	cwd, err := m.resolveCwd(body.Cwd)
	if err != nil {
		return nil, err
	}
	id := randomID("a_")
	dir := filepath.Join(m.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	name := body.Name
	if name == "" {
		name = id
	}
	a, err := m.newAgent(dir, Meta{ID: id, Name: name, Harness: body.Harness, Cwd: cwd, Owner: owner, Created: now, Updated: now})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Agent, error) {
		a.Stop()
		a.logf.Close()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := a.ensureRunning(ctx); err != nil {
		return fail(errf(502, "starting %s: %v", body.Harness, err))
	}
	if len(body.Config) > 0 {
		if err := a.SetConfig(ctx, body.Config, owner); err != nil {
			return fail(err)
		}
	}
	m.mu.Lock()
	m.agents[id] = a
	m.mu.Unlock()
	if body.Prompt != "" {
		if err := a.Send(ctx, body.Prompt, owner); err != nil {
			return a, err
		}
	}
	return a, nil
}

func (m *Manager) Get(id string) (*Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.agents[id]
	if !ok {
		return nil, errf(404, "no agent %s", id)
	}
	return a, nil
}

func (m *Manager) List() []*Agent {
	m.mu.Lock()
	out := make([]*Agent, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, a)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].meta.Created.Before(out[j].meta.Created) })
	return out
}

func (m *Manager) Delete(id string, purge bool) error {
	a, err := m.Get(id)
	if err != nil {
		return err
	}
	a.Stop()
	if !purge {
		return nil
	}
	m.mu.Lock()
	delete(m.agents, id)
	m.mu.Unlock()
	a.mu.Lock()
	a.logf.Close()
	a.mu.Unlock()
	return os.RemoveAll(a.dir)
}

func (m *Manager) StopAll() {
	var wg sync.WaitGroup
	for _, a := range m.List() {
		wg.Add(1)
		go func() { defer wg.Done(); a.Stop() }()
	}
	wg.Wait()
}
