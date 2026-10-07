package agent_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/llm"
)

func TestTheWriterSeesTheDataAndNoTools(t *testing.T) {
	research := agent.Research{Calls: []agent.Call{
		{Tool: "dividend_calendar", Arguments: json.RawMessage(`{"days":180}`), Result: "error: days is 180; it can be at most 120", Failed: true},
		{Tool: "dividend_calendar", Arguments: json.RawMessage(`{"days":10}`), Result: calendar},
	}}
	model := &scriptedModel{script: []llm.Message{says("MSFT goes ex-dividend on 8 October.")}, tokens: 900}

	today := time.Date(2026, 10, 7, 9, 0, 0, 0, time.Local)
	draft, err := (&agent.Writer{Model: model, Today: today}).Write(t.Context(), "What goes ex-dividend soon?", research)

	if err != nil {
		t.Fatal(err)
	}
	if draft.Text != "MSFT goes ex-dividend on 8 October." || draft.Tokens != 900 {
		t.Errorf("draft = %+v", draft)
	}
	req := model.seen[0]
	if len(req.Tools) != 0 {
		t.Errorf("the writer was offered %d tools, want none", len(req.Tools))
	}
	prompt := req.Messages[1].Content
	for _, want := range []string{"Today's date: 2026-10-07", "Question: What goes ex-dividend soon?", `Data from dividend_calendar {"days":10}:`, calendar} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// The refusal the research model saw is not data.
	if strings.Contains(prompt, "180") || strings.Contains(prompt, "incomplete") {
		t.Errorf("prompt shows the failed call or an early stop:\n%s", prompt)
	}
}

func TestTheWriterIsToldWhenResearchStoppedEarly(t *testing.T) {
	prompt := agent.WriteRequest("q", time.Time{}, agent.Research{Exhausted: agent.LimitTokens}).Messages[1].Content

	if strings.Contains(prompt, "Today") {
		t.Errorf("a zero Today still put a date in the prompt:\n%s", prompt)
	}
	for _, want := range []string{"No data was retrieved.", "its tokens budget ran out", "may be incomplete"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

// A reply with no text (here, a request for a tool the writer was never
// offered) is not a draft.
func TestAnEmptyReplyIsNotADraft(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`)}}

	_, err := (&agent.Writer{Model: model}).Write(t.Context(), "q", agent.Research{})

	if !errors.Is(err, agent.ErrNothingWritten) {
		t.Errorf("err = %v, want ErrNothingWritten", err)
	}
}
