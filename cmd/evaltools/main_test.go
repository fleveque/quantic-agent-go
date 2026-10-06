package main

import (
	"encoding/json"
	"testing"

	"github.com/fleveque/quantic-agent/internal/llm"
	"github.com/fleveque/quantic-agent/internal/tools"
)

func calls(name, args string, n int) llm.Message {
	m := llm.Message{Role: "assistant"}
	for range n {
		m.ToolCalls = append(m.ToolCalls, llm.ToolCall{Function: llm.FunctionCall{Name: name, Arguments: json.RawMessage(args)}})
	}
	return m
}

func TestJudge(t *testing.T) {
	window := Case{Name: "w", WantTool: "dividend_calendar", DaysMin: 5, DaysMax: 7}
	anyWindow := Case{Name: "a", WantTool: "dividend_calendar"}
	noTool := Case{Name: "n"}
	allowed := []tools.Tool{tools.DividendCalendar}

	tests := []struct {
		name               string
		c                  Case
		msg                llm.Message
		wantValid, wantRet bool
		wantReason         string
	}{
		{"right call, in range", window, calls("dividend_calendar", `{"days":7}`, 1), true, true, ""},
		{"right call, out of range", window, calls("dividend_calendar", `{"days":30}`, 1), true, false, "asked for 30 days, want 5–7"},
		{"days left out means 45", window, calls("dividend_calendar", `{}`, 1), true, false, "asked for 45 days, want 5–7"},
		{"any window accepted", anyWindow, calls("dividend_calendar", `{}`, 1), true, true, ""},
		{"answered instead", window, llm.Message{Content: "Probably MSFT."}, true, false, "answered without calling dividend_calendar"},
		{"no tool, none called", noTool, llm.Message{Content: "391"}, true, true, ""},
		{"tool called when none needed", noTool, calls("dividend_calendar", `{}`, 1), true, false, "called a tool for a question that needs none"},
		{"invented tool", window, calls("get_dividends", `{}`, 1), false, false, "called a tool that doesn't exist"},
		{"arguments that don't decode", window, calls("dividend_calendar", `{"days":"7"}`, 1), false, false, "tools: dividend_calendar arguments: json: cannot unmarshal string into Go struct field DividendCalendarArgs.days of type int"},
		{"two calls for one", window, calls("dividend_calendar", `{"days":7}`, 2), true, false, "made 2 calls where one was needed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := judge(tt.c, tt.msg, allowed)
			if o.Valid != tt.wantValid || o.Correct != tt.wantRet || o.Reason != tt.wantReason {
				t.Errorf("judge = valid %v, correct %v, reason %q; want %v, %v, %q",
					o.Valid, o.Correct, o.Reason, tt.wantValid, tt.wantRet, tt.wantReason)
			}
		})
	}
}

// The embedded cases are data a person edits, so they're checked like code.
func TestCasesAreWellFormed(t *testing.T) {
	var cases []Case
	if err := json.Unmarshal(casesJSON, &cases); err != nil {
		t.Fatalf("cases.json: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	names := map[string]bool{}
	for _, c := range cases {
		if c.Name == "" || c.Question == "" {
			t.Errorf("case %+v needs a name and a question", c)
		}
		if names[c.Name] {
			t.Errorf("duplicate case name %q", c.Name)
		}
		names[c.Name] = true
		if c.DaysMin > c.DaysMax || (c.WantTool == "" && c.DaysMax > 0) {
			t.Errorf("case %q has an impossible window %d–%d", c.Name, c.DaysMin, c.DaysMax)
		}
		if c.WantTool != "" && c.WantTool != tools.DividendCalendar.Name {
			t.Errorf("case %q wants %q, which is not an allowed tool", c.Name, c.WantTool)
		}
	}
}
