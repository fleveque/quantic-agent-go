// Command bench measures what the model server can actually do on the
// machine it is running on.
//
// It exists for design open question 9: on a 16GB card, does a 27B with some
// layers spilled into system RAM beat a 9B that fits entirely in VRAM? That
// is a measurement, not an argument, and the answer belongs to one machine,
// so the benchmark travels with the repo and runs where the agent will.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fleveque/quantic-agent/internal/llm"
)

// defaultModels are the candidates for design open question 9: the 9B that
// fits with room to spare, and Qwen3.8-27B at two sub-4-bit quantisations
// that Ollama's own library doesn't publish, pulled from Hugging Face.
const defaultModels = "qwen3.5:9b," +
	"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-IQ3_S," +
	"hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q3_K_XL"

// result is one (model, context length) measurement.
type result struct {
	Model         string  `json:"model"`
	ContextTokens int     `json:"context_tokens"`
	PromptTokens  int     `json:"prompt_tokens"`
	PromptRate    float64 `json:"prompt_tokens_per_second"`
	GenTokens     int     `json:"generated_tokens"`
	GenRate       float64 `json:"generated_tokens_per_second"`
	TotalSeconds  float64 `json:"total_seconds"`
	LoadSeconds   float64 `json:"load_seconds"`
	SizeBytes     int64   `json:"size_bytes"`
	VRAMBytes     int64   `json:"vram_bytes"`
	OnGPU         float64 `json:"fraction_on_gpu"`
	GenRequested  int     `json:"generated_tokens_requested"`
}

// defaultTimeout bounds each request, not the whole run. The slowest request
// measured so far took 94s (a 27B partly in system RAM at 64K, generating 128
// tokens), and -predict can ask for far more, so the margin is wide.
const defaultTimeout = 10 * time.Minute

func main() {
	// Ctrl-C stops the run and prints what was measured; a second one kills it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseURL := fs.String("ollama", envOr("OLLAMA_HOST", llm.DefaultBaseURL), "model server base URL")
	models := fs.String("models", defaultModels, "comma-separated models to measure")
	contexts := fs.String("contexts", "8192,32768,65536", "comma-separated context sizes in tokens")
	predict := fs.Int("predict", 128, "tokens to generate per measurement")
	asJSON := fs.Bool("json", false, "emit results as JSON instead of a table")
	timeout := fs.Duration("timeout", defaultTimeout, "give up on any single request after this long")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	wanted := splitList(*models)
	sizes, err := parseSizes(*contexts)
	if err != nil {
		fmt.Fprintln(stderr, "bench:", err)
		return 2
	}

	client := llm.New(*baseURL, "")
	listCtx, cancel := context.WithTimeout(ctx, *timeout)
	available, err := client.Models(listCtx)
	cancel()
	if err != nil {
		fmt.Fprintln(stderr, "bench:", err)
		return 1
	}
	present := make([]string, 0, len(available))
	for _, m := range available {
		present = append(present, m.Name)
	}

	var results []result
	// The label lets the inner loop stop the outer one.
models:
	for _, name := range wanted {
		if !slices.ContainsFunc(present, func(p string) bool { return strings.EqualFold(p, name) }) {
			fmt.Fprintf(stderr, "bench: %s is not on this server, skipping\n", name)
			continue
		}

		// One untimed call so the measurements below exclude loading the
		// weights, and so load time is reported once rather than smeared
		// across the first context size.
		loaded := llm.New(*baseURL, name)
		load, err := warmUp(ctx, loaded, sizes[0], *timeout)
		if err != nil {
			fmt.Fprintf(stderr, "bench: %s: %v\n", name, err)
			if stopRun(err) {
				break models
			}
			continue
		}

		for _, size := range sizes {
			r, err := measure(ctx, loaded, name, size, *predict, load, *timeout)
			if err != nil {
				fmt.Fprintf(stderr, "bench: %s at %d: %v\n", name, size, err)
				if stopRun(err) {
					break models
				}
				continue
			}
			results = append(results, r)
		}
	}

	// Interrupted: still report what was measured, then say so.
	code := 0
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "bench: stopped; the request in flight was cancelled")
		code = 130
	}

	if len(results) == 0 {
		fmt.Fprintln(stderr, "bench: nothing measured")
		return max(code, 1)
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			fmt.Fprintln(stderr, "bench:", err)
			return 1
		}
		return code
	}

	writeTable(stdout, results)
	return code
}

// stopRun reports whether an error means no later request can succeed either:
// the server went away, or the run itself was cancelled. A single request
// running out of time is not one of them; the next model may be faster.
func stopRun(err error) bool {
	return errors.Is(err, llm.ErrUnavailable) || errors.Is(err, context.Canceled)
}

// warmUp makes one untimed call so the measurements exclude loading the
// weights, and returns how long the load took so it is reported once rather
// than smeared across the first context size.
func warmUp(ctx context.Context, client *llm.Client, size int, timeout time.Duration) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := client.Generate(ctx, llm.GenerateRequest{
		Prompt:  "Reply with the single word: ready.",
		Think:   llm.Bool(false),
		Options: &llm.Options{NumPredict: 1, NumCtx: size},
	})
	if err != nil {
		return 0, err
	}
	return resp.LoadDuration, nil
}

// measure runs one generation and reports the rates the server itself
// counted. Ollama returns token counts and nanosecond durations per phase,
// so nothing here is timed with a wall clock that would include HTTP.
func measure(ctx context.Context, client *llm.Client, name string, size, predict int, load, timeout time.Duration) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := client.Generate(ctx, llm.GenerateRequest{
		Prompt: fillerPrompt(size * 8 / 10),
		Think:  llm.Bool(false),
		Options: &llm.Options{
			NumPredict: predict,
			NumCtx:     size,
		},
	})
	if err != nil {
		return result{}, err
	}

	r := result{
		Model:         name,
		ContextTokens: size,
		PromptTokens:  resp.PromptEvalCount,
		PromptRate:    rate(resp.PromptEvalCount, resp.PromptEvalDuration),
		GenTokens:     resp.EvalCount,
		GenRate:       rate(resp.EvalCount, resp.EvalDuration),
		TotalSeconds:  resp.TotalDuration.Seconds(),
		LoadSeconds:   load.Seconds(),
		GenRequested:  predict,
	}

	// Residency is only knowable while the model is still held, so ask
	// immediately after the generation rather than at the end of the run.
	running, err := client.Running(ctx)
	if err != nil {
		return r, nil // the measurement stands; residency is a bonus
	}
	for _, m := range running {
		if strings.EqualFold(m.Name, name) {
			r.SizeBytes, r.VRAMBytes, r.OnGPU = m.Size, m.SizeVRAM, m.OnGPU()
			break
		}
	}
	return r, nil
}

func writeTable(w io.Writer, results []result) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tCTX\tPROMPT tok/s\tGEN tok/s\tRESIDENT\tON GPU\tLOAD\tTOTAL")
	for _, r := range results {
		note := ""
		if r.GenRequested > 0 && r.GenTokens < r.GenRequested {
			note = fmt.Sprintf(" (stopped after %d of %d tokens: small sample)", r.GenTokens, r.GenRequested)
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f\t%.1f\t%.1f GB\t%s\t%.1fs\t%.1fs%s\n",
			r.Model, shortCount(r.ContextTokens), r.PromptRate, r.GenRate,
			float64(r.SizeBytes)/1e9, percent(r.OnGPU), r.LoadSeconds, r.TotalSeconds, note)
	}
	tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "ON GPU below 100% means layers spilled into system RAM: the model is larger than")
	fmt.Fprintln(w, "the card once its KV cache is counted. To find out whether cache quantisation helps,")
	fmt.Fprintln(w, "run this again with OLLAMA_KV_CACHE_TYPE=q8_0 set on the *server* and compare the")
	fmt.Fprintln(w, "largest context row. If nothing moves, flash attention isn't active for that model")
	fmt.Fprintln(w, "and the setting is being ignored.")
}

// fillerPrompt returns a prompt of roughly tokens tokens. It asks for a long
// answer on purpose: generation speed measured over two tokens is noise, so
// the model should run to the -predict limit. The nonce matters too: without
// it Ollama's prefix cache answers the second run for free and reports a
// prompt-processing rate that doesn't exist.
func fillerPrompt(tokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %016x. Read the notes below, then summarise them at length.\n\n", rand.Uint64())

	const line = "Dividend note: the payout was declared, the ex-date is set, and the yield moved with the price. "
	for b.Len() < tokens*4 { // ~4 characters per token; the measured count is reported
		b.WriteString(line)
	}
	return b.String()
}

func rate(tokens int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(tokens) / d.Seconds()
}

func percent(f float64) string {
	if f <= 0 {
		return "0% (CPU)"
	}
	return fmt.Sprintf("%.0f%%", f*100)
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

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseSizes(s string) ([]int, error) {
	var out []int
	for _, part := range splitList(s) {
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("context size %q is not a positive number of tokens", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no context sizes given")
	}
	return out, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
