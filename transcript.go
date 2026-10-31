package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderTranscript turns the history into markdown blocks (chunks of one
// message merged, one block per message, tool call or request) and returns
// the last n blocks.
func renderTranscript(evs []Event, n int) string {
	var blocks []string
	var cur *strings.Builder
	curKind := ""
	flush := func() {
		if cur != nil {
			blocks = append(blocks, cur.String())
		}
		cur, curKind = nil, ""
	}
	add := func(s string) { flush(); blocks = append(blocks, s) }

	for _, e := range evs {
		switch e.Type {
		case "user_message":
			add(fmt.Sprintf("**%s:** %s", e.By, e.Text))
		case "acp":
			var u struct {
				Kind    string `json:"sessionUpdate"`
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Title  string `json:"title"`
				Status string `json:"status"`
			}
			_ = json.Unmarshal(e.Update, &u)
			switch u.Kind {
			case "agent_message_chunk":
				if u.Content.Type != "text" {
					continue
				}
				if curKind != "agent" {
					flush()
					cur, curKind = &strings.Builder{}, "agent"
					cur.WriteString("**agent:** ")
				}
				cur.WriteString(u.Content.Text)
			case "tool_call":
				add(fmt.Sprintf("> tool: %s", u.Title))
			}
		case "permission_request":
			ids := make([]string, len(e.Options))
			for i, o := range e.Options {
				ids[i] = o.ID
			}
			var tc struct {
				Title string `json:"title"`
			}
			_ = json.Unmarshal(e.ToolCall, &tc)
			add(fmt.Sprintf("> permission %s: %s [%s]", e.ID, tc.Title, strings.Join(ids, ", ")))
		case "question":
			var q struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(e.Request, &q)
			add(fmt.Sprintf("> question %s: %s", e.ID, q.Message))
		case "permission_resolved":
			what := e.Option
			if e.Error != "" {
				what = e.Error
			}
			add(fmt.Sprintf("> %s answered %s: %s", e.By, e.ID, what))
		case "turn_end":
			if e.Error != "" {
				add("> turn failed: " + e.Error)
			}
		case "config_changed":
			add(fmt.Sprintf("> %s set %s", e.By, e.Values))
		case "state":
			if e.State == "error" {
				add("> error: " + e.Error)
			}
		}
	}
	flush()
	if n > 0 && len(blocks) > n {
		blocks = blocks[len(blocks)-n:]
	}
	return strings.Join(blocks, "\n\n") + "\n"
}
