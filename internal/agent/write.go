package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fleveque/quantic-agent/internal/llm"
)

// writePrompt is the standing instruction for the writing phase. The writer
// is shown the data and nothing else, and offered no tools.
const writePrompt = "You write short, factual answers about dividends and the companies that pay them. " +
	"Use only the data you are given: every company, date and figure you mention must appear in it. " +
	"Do not work out new figures from it, such as counts, sums, averages or durations. " +
	"If the data doesn't answer the question, say so. " +
	"Describe what the data shows. Do not recommend buying or selling anything."

// ErrNothingWritten means the writer's reply had no text: nothing to check,
// and nothing to keep.
var ErrNothingWritten = errors.New("agent: the model wrote nothing")

// Writer runs the writing phase: one model call, no tools.
type Writer struct {
	Model Model
}

// Draft is what the writing phase produced.
type Draft struct {
	Text      string
	Truncated bool // the model ran out of tokens mid-answer
	Tokens    int
}

// Write answers question from what research gathered.
func (w *Writer) Write(ctx context.Context, question string, research Research) (Draft, error) {
	resp, err := w.Model.Chat(ctx, WriteRequest(question, research))
	if err != nil {
		return Draft{}, err
	}
	tokens := resp.PromptEvalCount + resp.EvalCount
	if strings.TrimSpace(resp.Message.Content) == "" {
		return Draft{Tokens: tokens}, ErrNothingWritten
	}
	return Draft{
		Text:      resp.Message.Content,
		Truncated: resp.Truncated(),
		Tokens:    tokens,
	}, nil
}

// WriteRequest is the writer's whole view of the world: the standing
// instruction, the question, and each successful call's result labelled with
// the call that produced it. Failed calls are left out: they are errors the
// research model was shown, not data. Exported so the prompt can be
// inspected exactly as the model gets it.
func WriteRequest(question string, research Research) llm.ChatRequest {
	var b strings.Builder
	fmt.Fprintf(&b, "Question: %s\n\n", question)
	n := 0
	for _, c := range research.Calls {
		if c.Failed {
			continue
		}
		n++
		fmt.Fprintf(&b, "Data from %s %s:\n%s\n\n", c.Tool, c.Arguments, c.Result)
	}
	if n == 0 {
		b.WriteString("No data was retrieved.\n\n")
	}
	if research.Exhausted != "" {
		fmt.Fprintf(&b, "The research stopped before it was finished (its %s budget ran out), so the data may be incomplete. Say what it covers.\n", research.Exhausted)
	}
	return llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "system", Content: writePrompt},
			{Role: "user", Content: strings.TrimSpace(b.String())},
		},
		Think: llm.Bool(false),
	}
}
