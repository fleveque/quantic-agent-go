// Package agent runs the research loop: the model is offered tools, asks for
// the ones it needs, and answers from what they return.
//
// This is milestone 5's version: one question, a cap on tool calls, and a
// record of every call. Milestone 8 grows it into the full loop of design
// §3.1 (budgets for time and tokens, retries, phases), and the writing phase
// that follows it never gets tools at all.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fleveque/quantic-agent/internal/llm"
	"github.com/fleveque/quantic-agent/internal/mcp"
	"github.com/fleveque/quantic-agent/internal/tools"
)

// Model is what the loop needs from a language model. *llm.Client satisfies
// it. The interface lives here, with the code that uses it, and not in
// package llm: the consumer says what it needs, and tests can supply a fake.
type Model interface {
	Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
}

// ToolServer is what the loop needs from Quantic. *mcp.Client satisfies it.
type ToolServer interface {
	CallTool(ctx context.Context, name string, arguments any) (mcp.Result, error)
}

// The real clients must keep satisfying the interfaces above. These lines
// cost nothing at run time; if either client's method signature changes,
// they stop compiling, instead of a mismatch surfacing later.
var (
	_ Model      = (*llm.Client)(nil)
	_ ToolServer = (*mcp.Client)(nil)
)

// DefaultMaxCalls caps the tool calls one question may make. A question about
// one calendar needs one; the cap is what stops a model that keeps asking.
const DefaultMaxCalls = 4

// ErrTooManyCalls means the model was still asking for tools when the cap was
// reached. Design §3.2 treats running out of budget as an outcome of the run,
// not a crash: the calls made so far are still returned.
var ErrTooManyCalls = errors.New("agent: tool call limit reached before an answer")

// systemPrompt is the standing instruction for a research question. Prompts
// are code (design open question 6): they live here, reviewed like the rest.
const systemPrompt = "You answer questions about dividends and the companies that pay them. " +
	"Use the tools for every fact, date and figure; never rely on memory for them. " +
	"If the tools don't provide something, say so instead of guessing. " +
	"Describe what the data shows. Do not recommend buying or selling anything."

// Call records one tool call: what the model asked for and what it got back.
// It is the audit log of design N3 in miniature: from these, an answer can be
// traced to the data it was given.
type Call struct {
	Tool      string
	Arguments json.RawMessage
	Result    string // the tool's output, or the error the model was shown
	Failed    bool   // Result is an error the model was shown, not data
	Duration  time.Duration
}

// Answer is the outcome of one question.
type Answer struct {
	Text      string
	Calls     []Call
	Truncated bool // the model ran out of tokens mid-answer
}

// Researcher answers questions with a model and a tool server.
type Researcher struct {
	Model    Model
	Server   ToolServer
	Tools    []tools.Tool // the allowlist: the only tools offered, and the only ones run
	MaxCalls int          // 0 means DefaultMaxCalls
}

// Ask runs the loop for one question. A model mistake (an unknown tool,
// arguments that don't fit, a tool's refusal) is shown to the model as the
// tool's result so it can correct itself, and counts against the cap. A
// failure the model can't fix, such as the tool server being down or the
// context ending, stops the loop and is returned with the calls made so far.
func (r *Researcher) Ask(ctx context.Context, question string) (Answer, error) {
	req, err := r.FirstRequest(question)
	if err != nil {
		return Answer{}, err
	}
	limit := r.MaxCalls
	if limit == 0 {
		limit = DefaultMaxCalls
	}
	history := req.Messages
	var answer Answer

	for {
		resp, err := r.Model.Chat(ctx, llm.ChatRequest{Messages: history, Tools: req.Tools, Think: req.Think})
		if err != nil {
			return answer, err
		}
		if len(resp.Message.ToolCalls) == 0 {
			answer.Text = resp.Message.Content
			answer.Truncated = resp.Truncated()
			return answer, nil
		}

		// The model's request goes into the history unchanged, followed by
		// one tool message per call it made.
		history = append(history, resp.Message)
		for _, tc := range resp.Message.ToolCalls {
			if len(answer.Calls) == limit {
				return answer, ErrTooManyCalls
			}
			call, err := r.run(ctx, tc)
			answer.Calls = append(answer.Calls, call)
			if err != nil {
				return answer, err
			}
			history = append(history, llm.Message{Role: "tool", ToolName: call.Tool, Content: call.Result})
		}
	}
}

// FirstRequest is the request that opens the loop for a question: the
// standing instructions, the question, and the allowlisted tools. It is
// exported so an evaluation can show a model exactly what the loop shows it.
func (r *Researcher) FirstRequest(question string) (llm.ChatRequest, error) {
	defs, err := r.toolDefs()
	if err != nil {
		return llm.ChatRequest{}, err
	}
	return llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: question},
		},
		Tools: defs,
		Think: llm.Bool(false),
	}, nil
}

// run executes one tool call. The error it returns is only for failures the
// model can't fix; everything else becomes the call's Result.
func (r *Researcher) run(ctx context.Context, tc llm.ToolCall) (Call, error) {
	start := time.Now()
	call := Call{Tool: tc.Function.Name, Arguments: tc.Function.Arguments}
	refuse := func(format string, args ...any) (Call, error) {
		call.Result = "error: " + fmt.Sprintf(format, args...)
		call.Failed = true
		call.Duration = time.Since(start)
		return call, nil
	}

	i := slices.IndexFunc(r.Tools, func(t tools.Tool) bool { return t.Name == call.Tool })
	if i < 0 {
		return refuse("there is no tool called %q; the tools are: %s", call.Tool, r.toolNames())
	}
	args, err := r.Tools[i].DecodeArgs(call.Arguments)
	if err != nil {
		return refuse("%v", err)
	}

	res, err := r.Server.CallTool(ctx, call.Tool, args)
	var rpcErr *mcp.RPCError
	switch {
	case errors.As(err, &rpcErr):
		return refuse("%v", rpcErr) // the server rejected the request itself
	case err != nil:
		call.Duration = time.Since(start)
		return call, err
	case res.IsError:
		return refuse("%s", res.Text) // the tool ran and declined
	}
	call.Result = res.Text
	call.Duration = time.Since(start)
	return call, nil
}

func (r *Researcher) toolDefs() ([]llm.ToolDef, error) {
	defs := make([]llm.ToolDef, 0, len(r.Tools))
	for _, t := range r.Tools {
		schema, err := t.Schema()
		if err != nil {
			return nil, fmt.Errorf("agent: tool %s: %w", t.Name, err)
		}
		defs = append(defs, llm.ToolDef{Type: "function", Function: llm.FunctionDef{
			Name: t.Name, Description: t.Description, Parameters: schema,
		}})
	}
	return defs, nil
}

func (r *Researcher) toolNames() string {
	names := make([]string, len(r.Tools))
	for i, t := range r.Tools {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}
