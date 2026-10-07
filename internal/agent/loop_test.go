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
// keeps every request so a test can check what the model was shown. Each
// reply reports tokens tokens used (prompt and generated together).
type scriptedModel struct {
	script []llm.Message
	tokens int
	seen   []llm.ChatRequest
}

func (m *scriptedModel) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	m.seen = append(m.seen, req)
	if len(m.seen) > len(m.script) {
		return llm.ChatResponse{}, fmt.Errorf("model called %d times, script has %d replies", len(m.seen), len(m.script))
	}
	return llm.ChatResponse{Message: m.script[len(m.seen)-1], DoneReason: "stop", PromptEvalCount: m.tokens}, nil
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

func TestResearchCallsAToolUntilTheModelIsDone(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{"days":10}`), says("MSFT goes ex-dividend on 8 October.")}, tokens: 700}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}

	answer, err := newResearcher(model, server).Research(t.Context(), "What goes ex-dividend in the next 10 days?", agent.Research{})

	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if answer.Exhausted != "" || answer.Tokens != 1400 {
		t.Errorf("exhausted %q after %d tokens, want no budget run out and 2 × 700 tokens", answer.Exhausted, answer.Tokens)
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
func TestResearchShowsModelMistakesToTheModel(t *testing.T) {
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

			answer, err := newResearcher(model, server).Research(t.Context(), "q", agent.Research{})

			if err != nil {
				t.Fatalf("Research: %v", err)
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

func TestResearchShowsToolRefusalsToTheModel(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`), says("The calendar isn't available.")}}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: "needs authentication", IsError: true}}}

	answer, err := newResearcher(model, server).Research(t.Context(), "q", agent.Research{})

	if err != nil {
		t.Fatalf("Research: %v", err)
	}
	if c := answer.Calls[0]; !c.Failed || c.Result != "error: needs authentication" {
		t.Errorf("call = %+v, want the refusal recorded as a failed call", c)
	}
}

// A model that never stops asking, under each budget. Running out isn't an
// error: research ends with what it gathered, saying which budget ran out.
func TestResearchStopsWhenABudgetRunsOut(t *testing.T) {
	tests := []struct {
		name      string
		budget    agent.Budget
		calls     int
		exhausted agent.Limit
	}{
		{"calls", agent.Budget{Calls: 3, Tokens: 1_000_000}, 3, agent.LimitCalls},
		// 1000 tokens a reply: the third reply takes it to 3000, which
		// reaches the budget, so its request is never run.
		{"tokens", agent.Budget{Calls: 100, Tokens: 3000}, 2, agent.LimitTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &scriptedModel{tokens: 1000}
			for range 10 {
				model.script = append(model.script, asks("dividend_calendar", `{"days":7}`))
			}
			server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}
			r := newResearcher(model, server)
			r.Budget = tt.budget

			got, err := r.Research(t.Context(), "q", agent.Research{})

			if err != nil {
				t.Fatalf("err = %v, want running out to be a normal end", err)
			}
			if got.Exhausted != tt.exhausted || len(got.Calls) != tt.calls || len(server.got) != tt.calls {
				t.Errorf("exhausted %q after %d calls (%d reached the server), want %q after %d",
					got.Exhausted, len(got.Calls), len(server.got), tt.exhausted, tt.calls)
			}
		})
	}
}

// A failure the model can't fix ends the run, keeping the error's identity.
func TestResearchStopsWhenTheServerIsGone(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`)}}
	server := &fakeServer{err: fmt.Errorf("mcp: tools/call: %w", mcp.ErrUnavailable)}

	answer, err := newResearcher(model, server).Research(t.Context(), "q", agent.Research{})

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
func TestResearchShowsRPCErrorsToTheModel(t *testing.T) {
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{}`), says("ok")}}
	rpcErr := &mcp.RPCError{Code: -32602, Message: "Invalid params"}
	server := &fakeServer{err: rpcErr}

	answer, err := newResearcher(model, server).Research(t.Context(), "q", agent.Research{})

	if err != nil {
		t.Fatalf("Research: %v", err)
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

	if _, err := r.Research(t.Context(), "q", agent.Research{}); err != nil {
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

	_, err := r.Research(t.Context(), "q", agent.Research{})

	if !errors.Is(err, diskFull) {
		t.Errorf("err = %v, want the recording failure", err)
	}
	if len(model.seen) != 1 {
		t.Errorf("model asked %d times, want the run stopped after the unrecorded call", len(model.seen))
	}
}

// An interrupted run resumes: its recorded calls are replayed to the model,
// not made again, and they count against the budget.
func TestResearchResumesFromRecordedCalls(t *testing.T) {
	// Spare capacity, as a slice built by append usually has: room for
	// Research to write into, if it appended to this slice instead of a copy.
	calls := make([]agent.Call, 2, 8)
	calls[0] = agent.Call{Tool: "get_weather", Arguments: json.RawMessage(`{}`), Result: "error: no such tool", Failed: true}
	calls[1] = agent.Call{Tool: "dividend_calendar", Arguments: json.RawMessage(`{"days":10}`), Result: calendar}
	prior := agent.Research{Calls: calls, Tokens: 1500}
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{"days":30}`), says("done")}, tokens: 1000}
	server := &fakeServer{results: map[string]mcp.Result{"dividend_calendar": {Text: calendar}}}
	r := newResearcher(model, server)
	r.Budget = agent.Budget{Calls: 3}
	var seqs []int
	r.Record = func(ctx context.Context, seq int, c agent.Call) error {
		seqs = append(seqs, seq)
		return nil
	}

	got, err := r.Research(t.Context(), "q", prior)

	if err != nil {
		t.Fatal(err)
	}
	// Only the new call reached the server, and it was recorded after the
	// old ones.
	if len(server.got) != 1 || !slices.Equal(seqs, []int{2}) {
		t.Errorf("server got %v, recorded seqs %v; want one new call, seq 2", server.got, seqs)
	}
	if len(got.Calls) != 3 || got.Tokens != 3500 {
		t.Errorf("calls %d, tokens %d; want 3 calls and 1500 + 2 × 1000 tokens", len(got.Calls), got.Tokens)
	}
	// The model's first view: system, question, then the two recorded calls
	// as request and result.
	first := model.seen[0].Messages
	if len(first) != 6 || first[3].Content != "error: no such tool" || first[4].ToolCalls[0].Function.Name != "dividend_calendar" || first[5].Content != calendar {
		t.Errorf("replayed history = %+v", first)
	}
	// The caller's slice is left as it was, including the memory past its
	// length that shares its backing array.
	if spare := prior.Calls[:3][2]; spare.Tool != "" {
		t.Errorf("Research wrote %s into the caller's backing array", spare.Tool)
	}
}

// The resumed run's call budget is already spent: no new call is made.
func TestResumingWithTheBudgetSpent(t *testing.T) {
	prior := agent.Research{Calls: []agent.Call{{Tool: "dividend_calendar", Arguments: json.RawMessage(`{}`), Result: calendar}}}
	model := &scriptedModel{script: []llm.Message{asks("dividend_calendar", `{"days":30}`)}}
	server := &fakeServer{}
	r := newResearcher(model, server)
	r.Budget = agent.Budget{Calls: 1}

	got, err := r.Research(t.Context(), "q", prior)

	if err != nil || got.Exhausted != agent.LimitCalls || len(server.got) != 0 {
		t.Errorf("got %+v, %v; want calls exhausted and nothing sent", got, err)
	}
}

// A resumed run that had already spent its tokens doesn't ask the model again.
func TestResumingWithTheTokensSpent(t *testing.T) {
	model := &scriptedModel{}
	r := newResearcher(model, &fakeServer{})
	r.Budget = agent.Budget{Tokens: 3000}

	got, err := r.Research(t.Context(), "q", agent.Research{Tokens: 3200})

	if err != nil || got.Exhausted != agent.LimitTokens || len(model.seen) != 0 {
		t.Errorf("got %+v, %v after %d model calls; want tokens exhausted and no call", got, err, len(model.seen))
	}
}
