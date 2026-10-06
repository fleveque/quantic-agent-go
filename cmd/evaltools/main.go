// Command evaltools measures how well models choose and call tools, on a
// fixed set of questions whose right first move is known (cases.json).
//
// Decision 0005 says models are chosen by measurement on this agent's own
// tasks, and that milestone 5 leaves behind an evaluation any model name can
// be put through. This is the first part of it: does the model pick the right
// tool (or correctly pick none), with arguments that decode and make sense?
// It scores only the model's first reply, so it needs Ollama but not Quantic,
// and its verdicts don't depend on what the calendar holds today.
//
// Each case runs several times per model: sampling varies between runs, and
// a model that is right three times out of five is not the same as one that
// is right every time.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/llm"
	"github.com/fleveque/quantic-agent/internal/tools"
)

// The cases are compiled into the binary: the evaluation is code, versioned
// and reviewed with the rest, and runs the same wherever the binary does.
//
//go:embed cases.json
var casesJSON []byte

// Case is one question and its right first move.
type Case struct {
	Name     string `json:"name"`
	Question string `json:"question"`
	WantTool string `json:"want_tool"` // "" means the right move is not to call a tool
	DaysMin  int    `json:"days_min"`  // 0 and 0: any window is acceptable
	DaysMax  int    `json:"days_max"`
}

// Outcome is one run of one case.
type Outcome struct {
	Case    string `json:"case"`
	Correct bool   `json:"correct"`
	Valid   bool   `json:"valid"`  // a tool call that names a real tool and whose arguments decode
	Called  string `json:"called"` // the tool called, or "" for none
	Args    string `json:"args"`
	Reason  string `json:"reason,omitempty"` // why it isn't correct
}

// Score is one model's results.
type Score struct {
	Model    string    `json:"model"`
	Runs     int       `json:"runs"`
	Correct  int       `json:"correct"`
	Outcomes []Outcome `json:"outcomes"`
}

const defaultModels = "qwen3.5:9b,qwen3.6:35b,hf.co/unsloth/Qwen3.8-27B-GGUF:UD-IQ3_S"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("evaltools", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("ollama", envOr("OLLAMA_HOST", llm.DefaultBaseURL), "model server base URL")
	models := fs.String("models", defaultModels, "comma-separated models to evaluate")
	repeat := fs.Int("repeat", 5, "runs per case per model")
	timeout := fs.Duration("timeout", 10*time.Minute, "give up on any single request after this long")
	asJSON := fs.Bool("json", false, "emit results as JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var cases []Case
	if err := json.Unmarshal(casesJSON, &cases); err != nil {
		fmt.Fprintln(stderr, "evaltools: cases.json:", err)
		return 1
	}

	var scores []Score
	for _, name := range splitList(*models) {
		score, err := evaluate(ctx, llm.New(*baseURL, name), cases, *repeat, *timeout)
		if err != nil {
			fmt.Fprintf(stderr, "evaltools: %s: %v\n", name, err)
			if ctx.Err() != nil {
				return 130
			}
			continue
		}
		scores = append(scores, score)
	}
	if len(scores) == 0 {
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(scores)
		return 0
	}
	writeTable(stdout, scores, cases)
	return 0
}

// evaluate runs every case repeat times against one model.
func evaluate(ctx context.Context, model *llm.Client, cases []Case, repeat int, timeout time.Duration) (Score, error) {
	allowed := []tools.Tool{tools.DividendCalendar}
	r := &agent.Researcher{Model: model, Tools: allowed}
	score := Score{Model: model.Model()}

	for _, c := range cases {
		req, err := r.FirstRequest(c.Question)
		if err != nil {
			return score, err
		}
		for range repeat {
			resp, err := chat(ctx, model, req, timeout)
			if err != nil {
				return score, err // a model that can't answer at all has no score
			}
			o := judge(c, resp.Message, allowed)
			score.Outcomes = append(score.Outcomes, o)
			score.Runs++
			if o.Correct {
				score.Correct++
			}
		}
	}
	return score, nil
}

func chat(ctx context.Context, model *llm.Client, req llm.ChatRequest, timeout time.Duration) (llm.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return model.Chat(ctx, req)
}

// judge decides whether a model's first reply was the right move.
func judge(c Case, msg llm.Message, allowed []tools.Tool) Outcome {
	o := Outcome{Case: c.Name}
	if len(msg.ToolCalls) == 0 {
		o.Valid = true // answering directly is always a well-formed move
		o.Correct = c.WantTool == ""
		if !o.Correct {
			o.Reason = "answered without calling " + c.WantTool
		}
		return o
	}

	call := msg.ToolCalls[0]
	o.Called, o.Args = call.Function.Name, string(call.Function.Arguments)
	i := slices.IndexFunc(allowed, func(t tools.Tool) bool { return t.Name == o.Called })
	if i < 0 {
		o.Reason = "called a tool that doesn't exist"
		return o
	}
	args, err := allowed[i].DecodeArgs(call.Function.Arguments)
	if err != nil {
		o.Reason = err.Error()
		return o
	}
	o.Valid = true

	switch {
	case c.WantTool == "":
		o.Reason = "called a tool for a question that needs none"
	case o.Called != c.WantTool:
		o.Reason = "called the wrong tool"
	case len(msg.ToolCalls) > 1:
		o.Reason = fmt.Sprintf("made %d calls where one was needed", len(msg.ToolCalls))
	default:
		days := args.(tools.DividendCalendarArgs).Days
		if days == 0 {
			days = 45 // the server's default when the argument is left out
		}
		if c.DaysMax > 0 && (days < c.DaysMin || days > c.DaysMax) {
			o.Reason = fmt.Sprintf("asked for %d days, want %d–%d", days, c.DaysMin, c.DaysMax)
		} else {
			o.Correct = true
		}
	}
	return o
}

func writeTable(w io.Writer, scores []Score, cases []Case) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := []string{"CASE"}
	for _, s := range scores {
		header = append(header, s.Model)
	}
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, c := range cases {
		row := []string{c.Name}
		for _, s := range scores {
			var right, runs int
			for _, o := range s.Outcomes {
				if o.Case == c.Name {
					runs++
					if o.Correct {
						right++
					}
				}
			}
			row = append(row, fmt.Sprintf("%d/%d", right, runs))
		}
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	total := []string{"TOTAL"}
	for _, s := range scores {
		total = append(total, fmt.Sprintf("%d/%d (%.0f%%)", s.Correct, s.Runs, 100*float64(s.Correct)/float64(s.Runs)))
	}
	fmt.Fprintln(tw, strings.Join(total, "\t"))
	tw.Flush()

	// The reasons are the useful part when a model gets something wrong.
	for _, s := range scores {
		seen := map[string]bool{}
		for _, o := range s.Outcomes {
			key := o.Case + ": " + o.Reason
			if o.Correct || seen[key] {
				continue
			}
			seen[key] = true
			fmt.Fprintf(w, "\n%s · %s · %s", s.Model, o.Case, o.Reason)
			if o.Called != "" {
				fmt.Fprintf(w, " · %s %s", o.Called, o.Args)
			}
		}
	}
	fmt.Fprintln(w)
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
