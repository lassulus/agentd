package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// notifyPayload is what the notify webhook receives for one event.
type notifyPayload struct {
	Type  string    `json:"type"`
	Agent agentView `json:"agent"`
	Event Event     `json:"event"`
	// Text is the last transcript block: the request or question itself,
	// or for turn_end the agent's last message.
	Text string `json:"text"`
}

func (a *Agent) notifyLocked(e Event) {
	var body []byte
	for _, n := range a.m.cfg.Notify {
		if !slices.Contains(n.Events, e.Type) {
			continue
		}
		if body == nil {
			var err error
			body, err = json.Marshal(notifyPayload{
				Type:  e.Type,
				Agent: a.viewLocked(false),
				Event: e,
				Text:  strings.TrimSpace(renderTranscript(a.events, 1)),
			})
			if err != nil {
				slog.Error("notify", "agent", a.meta.ID, "err", err)
				return
			}
		}
		go n.post(body, a.meta.ID, e.Seq)
	}
}

func (n *Notify) post(body []byte, agent string, seq int64) {
	req, err := http.NewRequest("POST", n.URL, bytes.NewReader(body))
	if err != nil {
		slog.Error("notify", "agent", agent, "seq", seq, "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if n.secret != "" {
		mac := hmac.New(sha256.New, []byte(n.secret))
		mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("notify", "agent", agent, "seq", seq, "err", err)
		return
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		slog.Error("notify", "agent", agent, "seq", seq, "status", res.Status)
	}
}
