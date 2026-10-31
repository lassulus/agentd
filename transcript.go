package main

import (
	"encoding/json"
	"fmt"
	"sort"
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
	questions := map[string]json.RawMessage{} // question id -> elicitation request

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
			questions[e.ID] = e.Request
			add(fmt.Sprintf("> question %s: %s", e.ID, q.Message))
		case "permission_resolved":
			what := e.Option
			switch {
			case e.Error != "":
				what = e.Error
			case len(e.Answer) > 0:
				what = describeAnswer(questions[e.ID], e.Answer)
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

// describeAnswer renders a question answer as "<field title>: <value>",
// using the titles from the question's requested schema.
func describeAnswer(request, answer json.RawMessage) string {
	var req struct {
		Schema struct {
			Properties map[string]struct {
				Title string `json:"title"`
			} `json:"properties"`
		} `json:"requestedSchema"`
	}
	_ = json.Unmarshal(request, &req)
	var values map[string]json.RawMessage
	if json.Unmarshal(answer, &values) != nil {
		return string(answer)
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		v := string(values[k])
		var s string
		if json.Unmarshal(values[k], &s) == nil {
			v = s
		}
		if v == "" {
			continue
		}
		title := req.Schema.Properties[k].Title
		if title == "" {
			title = k
		}
		parts = append(parts, title+": "+v)
	}
	return strings.Join(parts, "; ")
}
