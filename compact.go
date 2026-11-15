package main

import (
	"encoding/json"
	"sort"
)

// compactor merges the history for display: consecutive text chunks of the
// agent's message (or thought) become one chunk, and a tool_call absorbs
// all later tool_call_updates of its id. Items keep the order of their
// first event. Events are added as the history grows, so a page costs
// only the events since the last one.
type compactor struct {
	n     int64 // events added
	items []compactItem
	tools map[string]int // toolCallId -> index of its tool_call item
	run   int            // 1 + index of the text run the last event went into; 0: none
}

// compactItem is one entry of the compacted history: an event, or several
// merged into one. first is the seq of its first event (the paging
// cursor), ev.Seq that of its last one.
type compactItem struct {
	first int64
	ev    Event
	kind  string            // a text run: agent_message_chunk or agent_thought_chunk
	text  []byte            // the run's text
	tool  []json.RawMessage // a tool call and its updates, merged by event()
}

func (c *compactor) add(e Event) {
	c.n++
	prevRun := c.run
	c.run = 0
	if e.Type == "acp" {
		var u struct {
			Kind       string `json:"sessionUpdate"`
			ToolCallID string `json:"toolCallId"`
		}
		_ = json.Unmarshal(e.Update, &u)
		switch u.Kind {
		case "agent_message_chunk", "agent_thought_chunk":
			var m struct {
				Content struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(e.Update, &m) != nil || m.Content.Type != "text" {
				break
			}
			if prevRun > 0 && c.items[prevRun-1].kind == u.Kind {
				it := &c.items[prevRun-1]
				it.text = append(it.text, m.Content.Text...)
				it.ev.Seq, it.ev.TS = e.Seq, e.TS
				c.run = prevRun
				return
			}
			c.items = append(c.items, compactItem{first: e.Seq, ev: e, kind: u.Kind, text: []byte(m.Content.Text)})
			c.run = len(c.items)
			return
		case "tool_call", "tool_call_update":
			if i, ok := c.tools[u.ToolCallID]; ok && u.Kind == "tool_call_update" {
				it := &c.items[i]
				it.tool = append(it.tool, e.Update)
				it.ev.Seq, it.ev.TS = e.Seq, e.TS
				return
			}
			// A tool_call, or an update of an unknown tool call.
			c.items = append(c.items, compactItem{first: e.Seq, ev: e, tool: []json.RawMessage{e.Update}})
			if u.Kind == "tool_call" {
				if c.tools == nil {
					c.tools = map[string]int{}
				}
				c.tools[u.ToolCallID] = len(c.items) - 1
			}
			return
		}
	}
	c.items = append(c.items, compactItem{first: e.Seq, ev: e})
}

// event returns the item as one event. A tool call's updates are merged
// in order (later fields win, null ones are skipped); rawOutput is dropped.
func (it *compactItem) event() Event {
	e := it.ev
	switch {
	case it.kind != "":
		e.Update, _ = json.Marshal(map[string]any{
			"sessionUpdate": it.kind,
			"content":       map[string]string{"type": "text", "text": string(it.text)},
		})
	case it.tool != nil:
		var f map[string]json.RawMessage
		_ = json.Unmarshal(it.tool[0], &f)
		for _, raw := range it.tool[1:] {
			var u map[string]json.RawMessage
			_ = json.Unmarshal(raw, &u)
			for k, v := range u {
				if k != "sessionUpdate" && string(v) != "null" {
					f[k] = v
				}
			}
		}
		delete(f, "rawOutput")
		e.Update, _ = json.Marshal(f)
	}
	return e
}

// page returns the last limit items that start before seq before (0: the
// newest ones), and the cursor for the page before them (0: there is none).
func (c *compactor) page(before int64, limit int) ([]Event, int64) {
	end := len(c.items)
	if before > 0 {
		end = sort.Search(end, func(i int) bool { return c.items[i].first >= before })
	}
	start := max(end-limit, 0)
	out := make([]Event, 0, end-start)
	for i := start; i < end; i++ {
		out = append(out, c.items[i].event())
	}
	var prev int64
	if start > 0 {
		prev = c.items[start].first
	}
	return out, prev
}
