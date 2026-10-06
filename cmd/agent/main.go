// Command agent is the quantic-agent daemon.
//
// It still has no scheduled tasks. What it can do so far: -check reports the
// model server and its models, -ask sends one prompt and prints the reply,
// and -research answers a question with Quantic's tools, printing each tool
// call it made to stderr.
//
// Exit status: 0 success, 1 failure (including -timeout running out), 2 wrong
// usage, 3 the model server wasn't there to answer, 4 a -research answer
// contains figures no tool returned (design N1), 130 stopped by Ctrl-C or
// SIGTERM. 3 means nothing was attempted, so a scheduler can simply run the
// same command again later (design §3.6). On 130 the request in flight was
// cancelled, and Ollama stops working on it too.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fleveque/quantic-agent/internal/agent"
	"github.com/fleveque/quantic-agent/internal/llm"
	"github.com/fleveque/quantic-agent/internal/mcp"
	"github.com/fleveque/quantic-agent/internal/provenance"
	"github.com/fleveque/quantic-agent/internal/tools"
)

// version is replaced at build time with -ldflags once there are releases
// (milestone 13). Until then every build reports "dev".
var version = "dev"

// defaultModel is the safe choice for the target hardware (design §4): it fits
// any 16GB card with room to spare. The primary candidate, Qwen3.8-27B at
// about 3.5 bits per weight, replaces it once cmd/bench confirms it on the
// real card. Development happens on a different machine, so both flags below
// read an environment variable first and nothing is baked in.
const defaultModel = "qwen3.5:9b"

// Exit codes, as documented at the top of this file.
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitUnavailable = 3
	exitUnverified  = 4
	exitInterrupted = 130 // 128 + SIGINT, the shell's convention for Ctrl-C
)

// defaultTimeout bounds one run. The slowest request measured on the target
// machine took 94s (a 27B partly in system RAM, 64K context) and the slowest
// cold load 31s, so five minutes leaves more than double. It must never be
// short enough to cut off a load: cancelling a load aborts it, and the next
// attempt starts from zero.
const defaultTimeout = 5 * time.Minute

func main() {
	// Ctrl-C and SIGTERM (what systemd sends to stop a service) cancel ctx,
	// and through it the request in flight.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Once that has happened, hand the signals back to the default handling,
	// so a second Ctrl-C kills the process instead of waiting for a clean
	// stop.
	context.AfterFunc(ctx, stop)

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with its dependencies passed in, returning the exit code.
//
// main itself is hard to test: it reads the real process arguments, writes to
// the real terminal, and os.Exit skips deferred calls. So main stays one line
// and everything worth testing lives here.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "print the version and exit")
	check := fs.Bool("check", false, "report the model server's version and exit")
	ask := fs.String("ask", "", "send one prompt to the model and print the reply")
	research := fs.String("research", "", "answer a question using Quantic's tools")
	mcpURL := fs.String("mcp", envOr("QUANTIC_MCP_URL", mcp.DefaultURL), "Quantic MCP server URL")
	baseURL := fs.String("ollama", envOr("OLLAMA_HOST", llm.DefaultBaseURL), "model server base URL")
	model := fs.String("model", envOr("QUANTIC_MODEL", defaultModel), "model to generate with")
	timeout := fs.Duration("timeout", defaultTimeout, "give up on the model server after this long (0: no limit)")

	if err := fs.Parse(args); err != nil {
		// The flag package has already written the problem and the usage
		// text to stderr. -h is a request, not a mistake.
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}

	if *showVersion {
		fmt.Fprintln(stdout, "quantic-agent", version)
		return exitOK
	}

	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	client := llm.New(*baseURL, *model)
	fail := func(err error) int { return report(stderr, err, *baseURL, *mcpURL, client.Model(), *timeout) }

	switch {
	case *check:
		serverVersion, err := client.Version(ctx)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(stdout, "ollama %s at %s\n", serverVersion, *baseURL)

		models, err := client.Models(ctx)
		if err != nil {
			return fail(err)
		}
		selected := false
		for _, m := range models {
			marker := " "
			if strings.EqualFold(m.Name, client.Model()) {
				marker, selected = "*", true
			}
			fmt.Fprintf(stdout, "%s %-26s %5.1f GB  %-6s %-7s ctx %-5s %s\n",
				marker, m.Name, float64(m.Size)/1e9, m.Details.ParameterSize,
				m.Details.QuantizationLevel, shortCount(m.Details.ContextLength),
				strings.Join(m.Capabilities, " "))
		}
		// The agent runs where its developer isn't sitting, so a model that
		// was never pulled has to be loud now rather than 404 mid-task. Names
		// compare case-insensitively because that is how Ollama resolves them.
		if !selected {
			return fail(llm.ErrModelNotFound)
		}
		return exitOK

	case *ask != "":
		// Thinking is suppressed: the agent wants the answer, and a
		// reasoning trace is text nothing downstream is allowed to publish.
		resp, err := client.Generate(ctx, llm.GenerateRequest{Prompt: *ask, Think: llm.Bool(false)})
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, resp.Response)
		fmt.Fprintf(stderr, "%s · %d tokens · %s%s\n",
			resp.Model, resp.EvalCount, resp.EvalDuration.Round(time.Millisecond), truncationNote(resp))
		return exitOK

	case *research != "":
		// The token, if any, comes only from the environment: a secret on
		// the command line ends up in shell history and in ps.
		server := mcp.New(*mcpURL, os.Getenv("QUANTIC_MCP_TOKEN"))
		if _, err := server.Initialize(ctx); err != nil {
			return fail(err)
		}
		r := &agent.Researcher{Model: client, Server: server, Tools: []tools.Tool{tools.DividendCalendar}}
		answer, err := r.Ask(ctx, *research)
		for _, c := range answer.Calls {
			fmt.Fprintf(stderr, "tool %s %s → %s (%s)\n", c.Tool, c.Arguments, callSummary(c), c.Duration.Round(time.Millisecond))
		}
		if err != nil {
			return fail(err)
		}
		fmt.Fprintln(stdout, answer.Text)
		if answer.Truncated {
			fmt.Fprintln(stderr, "agent: the answer was truncated: the model hit its token limit")
		}
		return verify(stderr, answer)
	}

	fmt.Fprintln(stdout, "quantic-agent: no tasks defined yet")
	return exitOK
}

// verify checks every figure in a research answer against the data its tool
// calls returned (design N1). The answer has already been printed, so a person
// can see it; the findings and the exit status say it can't be trusted.
func verify(stderr io.Writer, answer agent.Answer) int {
	var records []provenance.Record
	for _, c := range answer.Calls {
		if !c.Failed {
			records = append(records, provenance.Record{Tool: c.Tool, Result: c.Result})
		}
	}
	m, err := provenance.NewManifest(records...)
	if err != nil {
		fmt.Fprintln(stderr, "agent:", err)
		return exitFailed
	}
	findings := provenance.CheckProse(answer.Text, m)
	if len(findings) == 0 {
		return exitOK
	}
	fmt.Fprintf(stderr, "agent: %d figure(s) in the answer came from no tool result:\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(stderr, "  %s\n", f)
	}
	return exitUnverified
}

// callSummary describes a tool call's outcome in a few words for the trace.
func callSummary(c agent.Call) string {
	if c.Failed {
		return c.Result
	}
	return fmt.Sprintf("%d bytes", len(c.Result))
}

// report describes err and chooses the exit code. It decides by the kind of
// error, which the llm package exposes as values, never by matching the
// message text: messages are for people and can change.
func report(stderr io.Writer, err error, baseURL, mcpURL, model string, timeout time.Duration) int {
	switch {
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stderr, "agent: stopped; the request in flight was cancelled")
		return exitInterrupted

	case errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintf(stderr, "agent: gave up after %s (-timeout). A cold model can take half a minute to load, and a long generation longer.\n", timeout)
		return exitFailed

	case errors.Is(err, mcp.ErrUnavailable):
		fmt.Fprintf(stderr, "agent: no MCP server answering at %s.\n", mcpURL)
		fmt.Fprintln(stderr, "agent:", err)
		return exitUnavailable

	case errors.Is(err, mcp.ErrUnauthorized):
		fmt.Fprintln(stderr, "agent: the MCP server rejected QUANTIC_MCP_TOKEN. Unset it to connect anonymously, or create a new token.")
		return exitFailed

	case errors.Is(err, agent.ErrTooManyCalls):
		fmt.Fprintln(stderr, "agent: the model kept asking for tools and never answered:", err)
		return exitFailed

	case errors.Is(err, llm.ErrUnavailable):
		fmt.Fprintf(stderr, "agent: no model server answering at %s. Is Ollama running?\n", baseURL)
		fmt.Fprintln(stderr, "agent:", err)
		return exitUnavailable

	case errors.Is(err, llm.ErrModelNotFound):
		fmt.Fprintf(stderr, "agent: %s is not on this server. Pull it with: ollama pull %s\n", model, model)
		return exitFailed
	}

	fmt.Fprintln(stderr, "agent:", err)

	// A 5xx means the server itself failed, such as a model that couldn't be
	// loaded. The reason is in its log, not in the reply.
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 500 {
		fmt.Fprintln(stderr, "agent: the model server failed; its log has the cause (journalctl -u ollama)")
	}
	return exitFailed
}

// shortCount renders a context length the way model cards do: 262144 as 256K.
func shortCount(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dM", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dK", n/1024)
	default:
		return strconv.Itoa(n)
	}
}

func truncationNote(resp llm.GenerateResponse) string {
	if resp.Truncated() {
		return " · truncated: hit the token limit"
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
