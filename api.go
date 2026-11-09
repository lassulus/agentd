package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

type server struct {
	m    *Manager
	auth *authenticator
}

type ctxUser struct{}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f func(w http.ResponseWriter, r *http.Request) error) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			user, err := s.auth.user(r)
			if err == nil {
				r = r.WithContext(context.WithValue(r.Context(), ctxUser{}, user))
				err = f(w, r)
			}
			if err != nil {
				code := 500
				var he *httpError
				if errors.As(err, &he) {
					code = he.code
				}
				writeJSON(w, code, map[string]string{"error": err.Error()})
			}
		})
	}
	h("GET /v1/harnesses", s.harnesses)
	h("GET /v1/harnesses/{name}/config", s.getHarnessConfig)
	h("PATCH /v1/harnesses/{name}/config", s.patchHarnessConfig)
	h("POST /v1/agents", s.createAgent)
	h("GET /v1/agents", s.listAgents)
	h("GET /v1/agents/{id}", s.getAgent)
	h("DELETE /v1/agents/{id}", s.deleteAgent)
	h("POST /v1/agents/{id}/messages", s.sendMessage)
	h("POST /v1/agents/{id}/cancel", s.cancel)
	h("GET /v1/agents/{id}/history", s.history)
	h("GET /v1/agents/{id}/events", s.events)
	h("GET /v1/agents/{id}/transcript", s.transcript)
	h("GET /v1/agents/{id}/config", s.getConfig)
	h("PATCH /v1/agents/{id}/config", s.patchConfig)
	h("GET /v1/agents/{id}/permissions", s.permissions)
	h("POST /v1/agents/{id}/permissions/{req}", s.answer)
	return mux
}

func userOf(r *http.Request) string { s, _ := r.Context().Value(ctxUser{}).(string); return s }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errf(400, "invalid JSON body: %v", err)
	}
	return nil
}

func intParam(r *http.Request, name string, def int64) (int64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, errf(400, "%s must be a non-negative integer", name)
	}
	return n, nil
}

func (s *server) agent(r *http.Request) (*Agent, error) { return s.m.Get(r.PathValue("id")) }

func (s *server) harnesses(w http.ResponseWriter, r *http.Request) error {
	names := make([]string, 0, len(s.m.cfg.Harnesses))
	for n := range s.m.cfg.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]map[string]string, len(names))
	for i, n := range names {
		out[i] = map[string]string{"name": n}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *server) harnessName(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if _, ok := s.m.cfg.Harnesses[name]; !ok {
		return "", errf(404, "unknown harness %q", name)
	}
	return name, nil
}

func (s *server) getHarnessConfig(w http.ResponseWriter, r *http.Request) error {
	name, err := s.harnessName(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, s.m.HarnessConfig(name))
	return nil
}

func (s *server) patchHarnessConfig(w http.ResponseWriter, r *http.Request) error {
	name, err := s.harnessName(r)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := readJSON(r, &values); err != nil {
		return err
	}
	c, err := s.m.SetHarnessConfig(name, values)
	if err != nil {
		return err
	}
	writeJSON(w, 200, c)
	return nil
}

func (s *server) createAgent(w http.ResponseWriter, r *http.Request) error {
	var body createBody
	if err := readJSON(r, &body); err != nil {
		return err
	}
	a, err := s.m.Create(r.Context(), body, userOf(r))
	if err != nil {
		return err
	}
	writeJSON(w, 201, a.View(true))
	return nil
}

func (s *server) listAgents(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	all, state := q.Get("all") == "1", q.Get("state")
	out := []agentView{}
	for _, a := range s.m.List() {
		v := a.View(false)
		switch {
		case state != "" && v.State != state:
		case state == "" && !all && v.State == "stopped":
		default:
			out = append(out, v)
		}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *server) getAgent(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, a.View(true))
	return nil
}

func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request) error {
	if err := s.m.Delete(r.PathValue("id"), r.URL.Query().Get("purge") == "1"); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

func (s *server) sendMessage(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &body); err != nil {
		return err
	}
	if body.Text == "" {
		return errf(400, "text is required")
	}
	if err := a.Send(r.Context(), body.Text, userOf(r)); err != nil {
		return err
	}
	a.mu.Lock()
	seq := int64(len(a.events))
	a.mu.Unlock()
	writeJSON(w, 202, map[string]int64{"seq": seq})
	return nil
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	if err := a.Cancel(userOf(r)); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}

// history returns events with seq > since. With wait=N it long-polls up to
// N seconds for the first new event. Without since: the last limit events.
func (s *server) history(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	since := int64(-1)
	if r.URL.Query().Has("since") {
		if since, err = intParam(r, "since", 0); err != nil {
			return err
		}
	}
	limit, err := intParam(r, "limit", 200)
	if err != nil {
		return err
	}
	wait, err := intParam(r, "wait", 0)
	if err != nil {
		return err
	}
	deadline := time.After(time.Duration(wait) * time.Second)
	for {
		a.mu.Lock()
		evs := a.eventsAfter(since, int(limit))
		ch, state, last := a.changed, a.stateLocked(), int64(len(a.events))
		a.mu.Unlock()
		if len(evs) > 0 || wait == 0 {
			next := last
			if len(evs) > 0 {
				next = evs[len(evs)-1].Seq
			} else if since >= 0 {
				next = min(since, last)
			}
			writeJSON(w, 200, map[string]any{"events": evs, "next": next, "state": state})
			return nil
		}
		select {
		case <-ch:
		case <-deadline:
			wait = 0
		case <-r.Context().Done():
			return nil
		}
	}
}

// events streams the history as server-sent events, starting after since
// (or Last-Event-ID).
func (s *server) events(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	since, err := intParam(r, "since", 0)
	if err != nil {
		return err
	}
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		if since, err = strconv.ParseInt(id, 10, 64); err != nil {
			return errf(400, "bad Last-Event-ID")
		}
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		return errf(500, "streaming unsupported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fl.Flush()
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()
	for {
		a.mu.Lock()
		evs := a.eventsAfter(since, 1000)
		ch := a.changed
		a.mu.Unlock()
		for _, e := range evs {
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, b); err != nil {
				return nil
			}
			since = e.Seq
		}
		fl.Flush()
		if len(evs) > 0 {
			continue
		}
		select {
		case <-ch:
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return nil
			}
		case <-r.Context().Done():
			return nil
		}
	}
}

func (s *server) transcript(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	last, err := intParam(r, "last", 20)
	if err != nil {
		return err
	}
	a.mu.Lock()
	evs := append([]Event(nil), a.events...)
	a.mu.Unlock()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, err = io.WriteString(w, renderTranscript(evs, int(last)))
	return err
}

func (s *server) getConfig(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, a.View(true).Config)
	return nil
}

func (s *server) patchConfig(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	var values map[string]json.RawMessage
	if err := readJSON(r, &values); err != nil {
		return err
	}
	if err := a.SetConfig(r.Context(), values, userOf(r)); err != nil {
		return err
	}
	writeJSON(w, 200, a.View(true).Config)
	return nil
}

func (s *server) permissions(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	out := a.Pending()
	if out == nil {
		out = []Event{}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *server) answer(w http.ResponseWriter, r *http.Request) error {
	a, err := s.agent(r)
	if err != nil {
		return err
	}
	var body answerBody
	if err := readJSON(r, &body); err != nil {
		return err
	}
	if err := a.Answer(r.PathValue("req"), body, userOf(r)); err != nil {
		return err
	}
	w.WriteHeader(204)
	return nil
}
