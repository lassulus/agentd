package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func acpEvent(seq int64, update string) Event {
	return Event{Seq: seq, Type: "acp", Update: json.RawMessage(update)}
}

func jsonEqual(t *testing.T, got, want json.RawMessage) bool {
	t.Helper()
	if len(got) == 0 || len(want) == 0 {
		return len(got) == len(want)
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

func TestCompactHistory(t *testing.T) {
	msg := func(seq int64, kind, text string) Event {
		return acpEvent(seq, fmt.Sprintf(`{"sessionUpdate":%q,"content":{"type":"text","text":%q}}`, kind, text))
	}
	evs := []Event{
		{Seq: 1, Type: "user_message", Text: "hi"},
		msg(2, "agent_message_chunk", "a"),
		msg(3, "agent_message_chunk", "b"),
		msg(4, "agent_thought_chunk", "c"),
		msg(5, "agent_message_chunk", "d"),
		acpEvent(6, `{"sessionUpdate":"tool_call","toolCallId":"tc","title":"t","status":"pending","content":[1],"rawOutput":1}`),
		msg(7, "agent_message_chunk", "e"),
		acpEvent(8, `{"sessionUpdate":"tool_call_update","toolCallId":"tc","status":"completed","content":[2],"title":null,"rawOutput":"big"}`),
		msg(9, "agent_message_chunk", "f"), // not merged with e: the update came between
		{Seq: 10, Type: "state", State: "idle"},
		msg(11, "agent_message_chunk", "g"),
		acpEvent(12, `{"sessionUpdate":"tool_call_update","toolCallId":"unknown","status":"failed","rawOutput":"x"}`),
		acpEvent(13, `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"…"}}`),
	}
	text := func(kind, s string) string {
		return fmt.Sprintf(`{"sessionUpdate":%q,"content":{"type":"text","text":%q}}`, kind, s)
	}
	want := []struct {
		first, seq int64
		typ        string
		update     string
	}{
		{1, 1, "user_message", ""},
		{2, 3, "acp", text("agent_message_chunk", "ab")},
		{4, 4, "acp", text("agent_thought_chunk", "c")},
		{5, 5, "acp", text("agent_message_chunk", "d")},
		{6, 8, "acp", `{"sessionUpdate":"tool_call","toolCallId":"tc","title":"t","status":"completed","content":[2]}`},
		{7, 7, "acp", text("agent_message_chunk", "e")},
		{9, 9, "acp", text("agent_message_chunk", "f")},
		{10, 10, "state", ""},
		{11, 11, "acp", text("agent_message_chunk", "g")},
		{12, 12, "acp", `{"sessionUpdate":"tool_call_update","toolCallId":"unknown","status":"failed"}`},
		{13, 13, "acp", `{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"…"}}`},
	}
	var c compactor
	for _, e := range evs {
		c.add(e)
	}
	items := c.items
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for i, w := range want {
		it := items[i]
		e := it.event()
		if it.first != w.first || e.Seq != w.seq || e.Type != w.typ || !jsonEqual(t, e.Update, json.RawMessage(w.update)) {
			t.Errorf("item %d: first %d seq %d %s %s, want %+v", i, it.first, e.Seq, e.Type, e.Update, w)
		}
	}

	// Paging backwards by the first seq of the oldest item; the tool call
	// (first 6, seq 8) stays before the message that started at 7.
	pages := [][]int64{{11, 12, 13}, {7, 9, 10}, {4, 5, 6}, {1, 2}}
	before := int64(0)
	for i, firsts := range pages {
		page, prev := c.page(before, 3)
		var got []int64
		for _, e := range page {
			for _, it := range items {
				if it.ev.Seq == e.Seq {
					got = append(got, it.first)
				}
			}
		}
		wantPrev := int64(0)
		if i+1 < len(pages) {
			wantPrev = firsts[0]
		}
		if !reflect.DeepEqual(got, firsts) || prev != wantPrev {
			t.Fatalf("page %d (before=%d): firsts %v prev %d, want %v prev %d", i, before, got, prev, firsts, wantPrev)
		}
		before = prev
	}
}

type compactRes struct {
	Events []Event `json:"events"`
	Next   int64   `json:"next"`
	Before int64   `json:"before"`
	State  string  `json:"state"`
}

func TestCompactHistoryAPI(t *testing.T) {
	e := newTestEnv(t, "")
	var a agentView
	if c := e.do("POST", "/v1/agents", map[string]any{"harness": "fake", "cwd": "repo", "prompt": "tool one"}, &a); c != 201 {
		t.Fatalf("create: %d", c)
	}
	end := e.waitFor(a.ID, 0, turnEnd)
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "tool two"}, nil)
	e.waitFor(a.ID, end.Seq, turnEnd)
	last := e.lastSeq(a.ID)

	// The raw history is unchanged: every chunk and update on its own.
	var raw historyRes
	e.do("GET", fmt.Sprintf("/v1/agents/%s/history?since=0&limit=1000", a.ID), nil, &raw)
	counts := map[string]int{}
	for i, ev := range raw.Events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("raw event %d has seq %d", i, ev.Seq)
		}
		var u struct {
			Kind string `json:"sessionUpdate"`
		}
		_ = json.Unmarshal(ev.Update, &u)
		counts[u.Kind]++
		if u.Kind == "tool_call_update" && !strings.Contains(string(ev.Update), "rawOutput") {
			t.Errorf("raw update lost rawOutput: %s", ev.Update)
		}
	}
	if counts["agent_message_chunk"] != 4 || counts["agent_thought_chunk"] != 4 || counts["tool_call_update"] != 4 || raw.Next != last {
		t.Fatalf("raw history: %v next %d, want 4 chunks of each and next %d", counts, raw.Next, last)
	}

	var full compactRes
	if c := e.do("GET", "/v1/agents/"+a.ID+"/history?compact=1&limit=1000", nil, &full); c != 200 {
		t.Fatalf("compact: %d", c)
	}
	if full.Next != last || full.Before != 0 || full.State != "idle" {
		t.Fatalf("compact: next %d before %d state %s, want next %d", full.Next, full.Before, full.State, last)
	}
	// Per turn: one thought, the tool call with its updates merged, one message.
	var acp []string
	var maxSeq int64
	for _, ev := range full.Events {
		maxSeq = max(maxSeq, ev.Seq)
		if ev.Type != "acp" {
			continue
		}
		var u struct {
			Kind    string          `json:"sessionUpdate"`
			Status  string          `json:"status"`
			Title   string          `json:"title"`
			Content json.RawMessage `json:"content"`
			Raw     json.RawMessage `json:"rawOutput"`
		}
		_ = json.Unmarshal(ev.Update, &u)
		var c struct{ Text string }
		_ = json.Unmarshal(u.Content, &c)
		switch u.Kind {
		case "tool_call":
			if u.Raw != nil || u.Title != "run" || !strings.Contains(string(u.Content), `"output"`) {
				t.Errorf("tool call: %s", ev.Update)
			}
			acp = append(acp, "tool "+u.Status)
		default:
			acp = append(acp, u.Kind+" "+c.Text)
		}
		// The item's seq is that of its last raw event.
		if ev.Seq < 1 || ev.Seq > int64(len(raw.Events)) || !lastOfItem(raw.Events[ev.Seq-1], u.Kind) {
			t.Errorf("item %s: seq %d is not its last raw event", u.Kind, ev.Seq)
		}
	}
	wantACP := []string{
		"agent_thought_chunk hmm", "tool completed", "agent_message_chunk echo: tool one!",
		"agent_thought_chunk hmm", "tool completed", "agent_message_chunk echo: tool two!",
	}
	if !reflect.DeepEqual(acp, wantACP) || maxSeq != last {
		t.Fatalf("compacted: %q (max seq %d), want %q (max seq %d)", acp, maxSeq, wantACP, last)
	}

	// Paging backwards with before= yields the same items.
	var paged []Event
	before := int64(0)
	for range 100 {
		q := "/v1/agents/" + a.ID + "/history?compact=1&limit=3"
		if before > 0 {
			q += fmt.Sprintf("&before=%d", before)
		}
		var p compactRes
		e.do("GET", q, nil, &p)
		if len(p.Events) > 3 {
			t.Fatalf("page of %d items, limit 3", len(p.Events))
		}
		paged = append(p.Events, paged...)
		if before = p.Before; before == 0 {
			break
		}
	}
	if len(paged) != len(full.Events) {
		t.Fatalf("paged %d items, want %d", len(paged), len(full.Events))
	}
	for i := range paged {
		if paged[i].Seq != full.Events[i].Seq || !jsonEqual(t, paged[i].Update, full.Events[i].Update) {
			t.Errorf("paged item %d: %+v, want %+v", i, paged[i], full.Events[i])
		}
	}

	if c := e.do("GET", "/v1/agents/"+a.ID+"/history?compact=1&since=0", nil, nil); c != 400 {
		t.Errorf("compact with since: %d, want 400", c)
	}

	// Live events continue at next+1: no gap, no duplicate.
	var tail compactRes
	e.do("GET", "/v1/agents/"+a.ID+"/history?compact=1&limit=2", nil, &tail)
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/agents/%s/events?since=%d", e.srv.URL, a.ID, tail.Next), nil)
	req.Header.Set("Authorization", "Bearer secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	e.do("POST", "/v1/agents/"+a.ID+"/messages", map[string]string{"text": "tool three"}, nil)
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(nil, 1<<20)
	want := tail.Next + 1
	ended := false
	for !ended && sc.Scan() {
		line := sc.Text()
		if id, ok := strings.CutPrefix(line, "id: "); ok {
			if id != fmt.Sprint(want) {
				t.Fatalf("live event id %s, want %d", id, want)
			}
			want++
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev Event
			_ = json.Unmarshal([]byte(data), &ev)
			ended = ev.Type == "turn_end"
		}
	}
	if !ended {
		t.Fatalf("stream ended before turn_end: %v", sc.Err())
	}

	// agentd extends its compacted history as events arrive; the result is
	// the same as compacting all events at once.
	e.do("GET", fmt.Sprintf("/v1/agents/%s/history?since=0&limit=1000", a.ID), nil, &raw)
	var c compactor
	for _, ev := range raw.Events {
		c.add(ev)
	}
	fresh, _ := c.page(0, 1000)
	e.do("GET", "/v1/agents/"+a.ID+"/history?compact=1&limit=1000", nil, &full)
	if len(full.Events) != len(fresh) {
		t.Fatalf("compacted %d items, fresh %d", len(full.Events), len(fresh))
	}
	for i := range fresh {
		if full.Events[i].Seq != fresh[i].Seq || !jsonEqual(t, full.Events[i].Update, fresh[i].Update) {
			t.Errorf("item %d: %+v, fresh %+v", i, full.Events[i], fresh[i])
		}
	}
}

// lastOfItem reports whether raw event ev can be the last one merged into
// an item of kind: a tool call's last event is its last update.
func lastOfItem(ev Event, kind string) bool {
	var u struct {
		Kind string `json:"sessionUpdate"`
	}
	_ = json.Unmarshal(ev.Update, &u)
	if kind == "tool_call" {
		return u.Kind == "tool_call_update"
	}
	return u.Kind == kind
}
