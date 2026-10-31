package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranscriptShowsQuestionAnswers(t *testing.T) {
	req := json.RawMessage(`{"message":"Pick","requestedSchema":{"type":"object","properties":{
		"q0":{"type":"string","title":"Pick a fruit","oneOf":[{"const":"apple"},{"const":"pear"}]},
		"q0__other":{"type":"string","title":"Other (type your own)"},
		"q1":{"type":"string","title":"Pick a drink"}}}}`)
	evs := []Event{
		{Type: "question", ID: "q_1", Request: req},
		{Type: "permission_resolved", ID: "q_1", By: "lass", Option: "accept",
			Answer: json.RawMessage(`{"q0":"pear","q0__other":"","q1":"tea"}`)},
		{Type: "question", ID: "q_2", Request: req},
		{Type: "permission_resolved", ID: "q_2", By: "lass", Option: "decline"},
	}
	tr := renderTranscript(evs, 0)
	for _, want := range []string{
		"> lass answered q_1: Pick a fruit: pear; Pick a drink: tea",
		"> lass answered q_2: decline",
	} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript lacks %q:\n%s", want, tr)
		}
	}
}
