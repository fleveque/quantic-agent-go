package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/llm"
	"github.com/fleveque/quantic-agent/internal/mcp"
	"github.com/fleveque/quantic-agent/internal/tools"
)

// scriptedModel replies with its script, one message per Chat call, and
// keeps every request so a test can check what the model was shown.
type scriptedModel struct {
	script []llm.Message
	seen   []llm.ChatRequest
}

func (m *scriptedModel) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	m.seen = append(m.seen, req)
	if len(m.seen) > len(m.script) {
		return llm.ChatResponse{}, fmt.Errorf("model called %d times, script has %d replies", len(m.seen), len(m.script))
	}
	return llm.ChatResponse{Message: m.script[len(m.seen)-1], DoneReason: "stop"}, nil
}

// fakeServer answers tool calls by name and records the arguments it got.
type fakeServer struct {
	results map[string]mcp.Result
	err     error
	got     []any
}

func (s *fakeServer) CallTool(ctx context.Context, name string, args any) (mcp.Result, error) {
	s.got = append(s.got, args)
	if s.err != nil {
		return mcp.Result{}, s.err
	}
	return s.results[name], nil
}

func asks(name, args string) llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{Function: llm.FunctionCall{Name: name, Arguments: json.RawMessage(args)}},
	}}
}

func says(text string) llm.Message { return llm.Message{Role: "assistant", Content: text} }

const calendar = `{"from":"2026-10-06","days":10,"stocks":[{"symbol":"MSFT","ex_dividend_date":"2026-10-08"}]}`

func newResearcher(m agent.Model, s agent.ToolServer) *agent.Researcher {
	return &agent.Researcher{Model: m, Server: s, Tools: []tools.Tool{tools.DividendCalendar}}
}

func TestAskCallsAToolThenAnswers(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{"days":10}`), says("MSFT goes ex-dividend on 8 October.")}}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}

	answer, err := newResearcher(model, server).Ask(t.Context(), "What goes ex-dividend in the next 10 days?")

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if answer.Text != "MSFT goes ex-dividend on 8 October." {
		t.Errorf("answer = %q", answer.Text)
	}
	// The arguments reached the server as the tool's own struct, decoded.
	if len(server.got) != 1 || server.got[0] != (tools.DividendCalendarArgs{Days: 10}) {
		t.Errorf("server got %#v, want DividendCalendarArgs{Days: 10}", server.got)
	}
	if len(answer.Calls) != 1 || answer.Calls[0].Result != calendar || answer.Calls[0].Failed {
		t.Errorf("calls = %+v, want one successful call recording the calendar", answer.Calls)
	}

	// Second turn: the model saw its own request, then the tool's output.
	history := model.seen[1].Messages
	last := history[len(history)-1]
	if last.Role != "tool" || last.ToolName != "dividend_calendar" || last.Content != calendar {
		t.Errorf("last message = %+v, want the tool result", last)
	}
	if len(history[len(history)-2].ToolCalls) != 1 {
		t.Errorf("the model's tool request is missing from the history")
	}
	// The tool was offered with the schema derived from its struct.
	if def := model.seen[0].Tools; len(def) != 1 || def[0].Function.Name != "dividend_calendar" {
		t.Errorf("tools offered = %+v", def)
	}
}

// A model mistake is shown to the model as the tool's result, so it can
// correct itself, and the server is never called with a bad request.
func TestAskShowsModelMistakesToTheModel(t *testing.T) {
	tests := []struct {
		name, tool, args, want string
	}{
		{"unknown tool", "get_weather", `{}`, `error: there is no tool called "get_weather"; the tools are: dividend_calendar`},
		{"wrong type", "dividend_calendar", `{"days":"ten"}`, "error: tools: dividend_calendar arguments: json: cannot unmarshal string"},
		{"unknown argument", "dividend_calendar", `{"days":10,"sector":"Utilities"}`, `unknown field "sector"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &scriptedModel{script: []llm.Message{asks(tt.tool, tt.args), says("Sorry.")}}
			server := &fakeServer{}

			answer, err := newResearcher(model, server).Ask(t.Context(), "q")

			if err != nil {
				t.Fatalf("Ask: %v", err)
			}
			if len(server.got) != 0 {
				t.Errorf("server was called with %v, want no call for a bad request", server.got)
			}
			call := answer.Calls[0]
			if !call.Failed || !strings.Contains(call.Result, tt.want) {
				t.Errorf("call = %+v, want a failed call mentioning %q", call, tt.want)
			}
			shown := model.seen[1].Messages[len(model.seen[1].Messages)-1]
			if shown.Content != call.Result {
				t.Errorf("model was shown %q, want %q", shown.Content, call.Result)
			}
		})
	}
}

func TestAskShowsToolRefusalsToTheModel(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`), says("The calendar isn't available.")}}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: "needs authentication", IsError: true}}}

	answer, err := newResearcher(model, server).Ask(t.Context(), "q")

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if c := answer.Calls[0]; !c.Failed || c.Result != "error: needs authentication" {
		t.Errorf("call = %+v, want the refusal recorded as a failed call", c)
	}
}

func TestAskStopsAtTheCallLimit(t *testing.T) {
	// A model that never stops asking.
	model := &scriptedModel{}
	for range 10 {
		model.script = append(model.script, asks("dividend_calendar", `{"days":7}`))
	}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}
	r := newResearcher(model, server)
	r.MaxCalls = 3

	answer, err := r.Ask(t.Context(), "q")

	if !errors.Is(err, agent.ErrTooManyCalls) {
		t.Fatalf("err = %v, want ErrTooManyCalls", err)
	}
	if len(answer.Calls) != 3 || len(server.got) != 3 {
		t.Errorf("made %d calls (%d reached the server), want exactly 3", len(answer.Calls), len(server.got))
	}
}

// A failure the model can't fix ends the run, keeping the error's identity.
func TestAskStopsWhenTheServerIsGone(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`)}}
	server := &fakeServer{err: fmt.Errorf("mcp: tools/call: %w", mcp.ErrUnavailable)}

	answer, err := newResearcher(model, server).Ask(t.Context(), "q")

	if !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("err = %v, want mcp.ErrUnavailable", err)
	}
	if len(model.seen) != 1 {
		t.Errorf("model was asked %d times, want the loop to stop after the failed call", len(model.seen))
	}
	if len(answer.Calls) != 1 {
		t.Errorf("calls = %+v, want the failed call still recorded", answer.Calls)
	}
}

// A protocol error from the server is the request's fault, so the model sees it.
func TestAskShowsRPCErrorsToTheModel(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`), says("ok")}}
	rpcErr := &mcp.RPCError{Code: -32602, Message: "Invalid params"}
	server := &fakeServer{err: rpcErr}

	answer, err := newResearcher(model, server).Ask(t.Context(), "q")

	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if c := answer.Calls[0]; !c.Failed || !strings.Contains(c.Result, "Invalid params") {
		t.Errorf("call = %+v, want the protocol error shown to the model", c)
	}
}

func TestEveryCallIsRecordedAsItHappens(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("get_weather", `{}`), asks("dividend_calendar", `{"days":10}`), says("done")}}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}
	var recorded []string
	r := newResearcher(model, server)
	r.Record = func(ctx context.Context, seq int, c agent.Call) error {
		recorded = append(recorded, fmt.Sprintf("%d:%s:%v", seq, c.Tool, c.Failed))
		return nil
	}

	if _, err := r.Ask(t.Context(), "q"); err != nil {
		t.Fatal(err)
	}
	// The refused call is recorded too: the audit log shows what was tried.
	if want := []string{"0:get_weather:true", "1:dividend_calendar:false"}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

func TestARunStopsIfACallCantBeRecorded(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`), says("done")}}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}
	r := newResearcher(model, server)
	diskFull := errors.New("disk full")
	r.Record = func(context.Context, int, agent.Call) error { return diskFull }

	_, err := r.Ask(t.Context(), "q")

	if !errors.Is(err, diskFull) {
		t.Errorf("err = %v, want the recording failure", err)
	}
	if len(model.seen) != 1 {
		t.Errorf("model asked %d times, want the run stopped after the unrecorded call", len(model.seen))
	}
}
