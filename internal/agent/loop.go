// Package agent answers a question in two phases (design §3.1). Research is
// a loop: the model is offered tools, asks for the ones it needs, and sees
// what they return, within a budget. Writing is one model call with no tools
// at all: it gets the question and the data research gathered, and nothing
// else, so it can't fetch, and it can't wander.
package agent

import (
	"cmp"
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

// Budget bounds the research phase (design §3.2). Running out is not a
// failure: research stops, and writing goes ahead with what was gathered.
// The third bound, wall-clock time, is the context's deadline.
type Budget struct {
	Calls  int // tool calls; 0 means DefaultBudget.Calls
	Tokens int // tokens the model processes and generates; 0 means DefaultBudget.Tokens
}

// DefaultBudget is a budget for one question. A question about one calendar
// needs one call and about 2,000 tokens (measured, docs/benchmarks); this
// leaves room for a model that corrects itself a few times, and stops one
// that keeps asking.
var DefaultBudget = Budget{Calls: 4, Tokens: 16_000}

// Limit names the budget that stopped research early.
type Limit string

const (
	LimitCalls  Limit = "calls"
	LimitTokens Limit = "tokens"
)

// systemPrompt is the standing instruction for the research phase. Prompts
// are code (design open question 6): they live here, reviewed like the rest.
// The tool-call evaluation (cmd/evaltools) measures models against this
// exact text, so changing it means measuring again.
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

// Research is what the research phase gathered: the calls it made, in
// order, the tokens it used, and the budget that stopped it, if one did.
type Research struct {
	Calls     []Call
	Tokens    int
	Exhausted Limit // "" when the model finished on its own
}

// Researcher runs the research phase with a model and a tool server.
type Researcher struct {
	Model  Model
	Server ToolServer
	Tools  []tools.Tool // the allowlist: the only tools offered, and the only ones run
	Budget Budget

	// Record, if set, is called after every tool call, successful or not,
	// with its position in the run. It is how calls reach the audit log
	// (design N3) as they happen. If it fails, the run stops: a call that
	// can't be recorded is a call that can't be audited.
	Record func(ctx context.Context, seq int, c Call) error
}

// Research runs the loop for one question until the model stops asking for
// tools or a budget runs out. A model mistake (an unknown tool, arguments
// that don't fit, a tool's refusal) is shown to the model as the tool's
// result so it can correct itself, and counts against the budget. A failure
// the model can't fix, such as the tool server being down or the context
// ending, stops the loop and is returned with what was gathered so far.
//
// prior is research already done for this question, as recorded by an
// earlier, interrupted run; its zero value starts afresh. Its calls are
// replayed to the model as if just made, without calling the tools again, and
// count against the budget like new ones.
//
// When the model stops asking, whatever it says is discarded: the answer is
// the writing phase's job.
func (r *Researcher) Research(ctx context.Context, question string, prior Research) (Research, error) {
	req, err := r.FirstRequest(question)
	if err != nil {
		return prior, err
	}
	budget := Budget{
		Calls:  cmp.Or(r.Budget.Calls, DefaultBudget.Calls),
		Tokens: cmp.Or(r.Budget.Tokens, DefaultBudget.Tokens),
	}
	history := append(req.Messages, replay(prior.Calls)...)
	research := prior
	research.Calls = slices.Clone(prior.Calls) // appending must not write into the caller's slice

	for {
		// Checked before asking (a resumed run may have spent its tokens
		// already) and again after: a reply that crosses the budget has its
		// requests dropped rather than run.
		if research.Tokens >= budget.Tokens {
			research.Exhausted = LimitTokens
			return research, nil
		}
		resp, err := r.Model.Chat(ctx, llm.ChatRequest{Messages: history, Tools: req.Tools, Think: req.Think})
		if err != nil {
			return research, err
		}
		research.Tokens += resp.PromptEvalCount + resp.EvalCount
		if len(resp.Message.ToolCalls) == 0 {
			return research, nil
		}
		if research.Tokens >= budget.Tokens {
			research.Exhausted = LimitTokens
			return research, nil
		}

		// The model's request goes into the history unchanged, followed by
		// one tool message per call it made.
		history = append(history, resp.Message)
		for _, tc := range resp.Message.ToolCalls {
			if len(research.Calls) == budget.Calls {
				research.Exhausted = LimitCalls
				return research, nil
			}
			call, err := r.run(ctx, tc)
			research.Calls = append(research.Calls, call)
			if r.Record != nil {
				if recErr := r.Record(ctx, len(research.Calls)-1, call); recErr != nil {
					return research, fmt.Errorf("agent: recording call: %w", recErr)
				}
			}
			if err != nil {
				return research, err
			}
			history = append(history, llm.Message{Role: "tool", ToolName: call.Tool, Content: call.Result})
		}
	}
}

// replay turns recorded calls back into the conversation that produced them:
// for each, the model's request and the tool's result. A model that asked for
// two tools in one message gets them back as two messages, one call each,
// which says the same thing.
func replay(calls []Call) []llm.Message {
	var history []llm.Message
	for _, c := range calls {
		history = append(history,
			llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
				{Function: llm.FunctionCall{Name: c.Tool, Arguments: c.Arguments}},
			}},
			llm.Message{Role: "tool", ToolName: c.Tool, Content: c.Result},
		)
	}
	return history
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
