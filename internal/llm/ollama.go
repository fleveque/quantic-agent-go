// Package llm is the client for the local model server.
//
// The agent generates through Ollama (design §4): one HTTP call per
// generation, never streamed, so a reply is one JSON object rather than a
// sequence of them. Tool schemas arrive in milestone 5.
//
// Every call takes a context.Context, and the caller's context is the only
// limit on how long a call may take: the client sets no timeout of its own.
// Cancelling it ends the request, and Ollama then stops working on it too:
// a cancelled generation frees the GPU within about a second. Cancelling
// while a model is still loading aborts the load, so a deadline must allow
// for a cold start.
//
// Failures come back as errors a caller can tell apart without reading their
// text: ErrUnavailable when the server isn't there to answer, ErrModelNotFound
// when it is but lacks the model, *APIError for any other refusal, and the
// context's own error (context.DeadlineExceeded or context.Canceled) when the
// caller stopped waiting. All of them arrive wrapped, so test for them with
// errors.Is and errors.As.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/fleveque/quantic-agent/internal/netx"
)

// DefaultBaseURL is where Ollama listens unless told otherwise.
const DefaultBaseURL = "http://localhost:11434"

// The kinds of failure a caller acts on differently. They are always
// returned wrapped in more context, so compare with errors.Is, never ==.
var (
	// ErrUnavailable means no reply came back because the server wasn't there
	// to give one: nothing listening at the address, or the connection
	// dropped before a response. That is what a stopped or restarting Ollama
	// looks like, and design §3.6 treats it as "wait and retry", not as a
	// failure of the task.
	ErrUnavailable = errors.New("model server unavailable")

	// ErrModelNotFound means the server answered but has no such model: it
	// was never pulled on this machine, or the name is misspelt.
	ErrModelNotFound = errors.New("model not found")
)

// APIError is a reply whose status wasn't 200 OK: the server was reached and
// said no. errors.As pulls it out of a wrapped chain when a caller needs the
// details, such as the status code.
type APIError struct {
	Method     string
	Path       string
	StatusCode int

	// Message is the server's explanation: the "error" field of a JSON body,
	// or the raw text of a body that isn't JSON. Empty if there was none.
	Message string

	// Err is the sentinel for a failure the client recognises
	// (ErrModelNotFound), or nil. Unwrap exposes it, so errors.Is(err,
	// ErrModelNotFound) sees through an *APIError.
	Err error
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("llm: %s %s: %d %s", e.Method, e.Path, e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Unwrap is what errors.Is and errors.As call to look inside an error.
func (e *APIError) Unwrap() error { return e.Err }

// Client is a handle on one Ollama server and one model.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// New returns a Client for model on the server at baseURL. A baseURL without
// a scheme gets http:// — OLLAMA_HOST is conventionally written "host:port",
// which is not a URL http.NewRequest will accept.
func New(baseURL, model string) *Client {
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		// No Timeout: how long a call may take is the caller's decision,
		// carried by the context each method takes.
		http: &http.Client{},
	}
}

// Model reports the model this client generates with.
func (c *Client) Model() string { return c.model }

// GenerateRequest is the part of a generation the caller varies. The model
// name and the stream setting belong to the client, not to the call.
type GenerateRequest struct {
	Prompt string

	// Think turns a thinking model's reasoning pass on or off. It is a
	// pointer because "off" and "unsaid" are different requests: nil leaves
	// the model's own default alone, while Bool(false) actively suppresses
	// the reasoning pass. A plain bool could only express one of them.
	Think *bool

	Options *Options
}

// Options are Ollama's per-request knobs.
//
// NumCtx is the one that bites: the server uses a 4096-token window unless
// asked for more, whatever the model itself supports, and a longer prompt is
// truncated to fit. A research context has to ask. The pointers are deliberate: 0 is a
// meaningful temperature and a meaningful seed, so those fields cannot use
// omitempty on a plain value without losing the ability to send zero.
type Options struct {
	NumPredict  int      `json:"num_predict,omitempty"`
	NumCtx      int      `json:"num_ctx,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
}

// generateBody is the wire format of POST /api/generate.
type generateBody struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`

	// Stream must be sent even when false. Ollama streams by default, and
	// `json:"stream,omitempty"` would drop the false and hand us a sequence
	// of JSON objects that Decode reads only the first of.
	Stream bool `json:"stream"`

	Think   *bool    `json:"think,omitempty"`
	Options *Options `json:"options,omitempty"`
}

// GenerateResponse is one non-streamed reply from /api/generate.
//
// The duration fields are nanosecond counts on the wire, which is exactly
// what a time.Duration is, so they decode straight into one.
type GenerateResponse struct {
	Model      string    `json:"model"`
	CreatedAt  time.Time `json:"created_at"`
	Response   string    `json:"response"`
	Thinking   string    `json:"thinking"`
	Done       bool      `json:"done"`
	DoneReason string    `json:"done_reason"`

	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`

	TotalDuration      time.Duration `json:"total_duration"`
	LoadDuration       time.Duration `json:"load_duration"`
	PromptEvalDuration time.Duration `json:"prompt_eval_duration"`
	EvalDuration       time.Duration `json:"eval_duration"`
}

// Truncated reports whether the model stopped because it ran out of token
// budget rather than because it finished. A truncated draft is a broken
// draft: half a table is worse than no table.
func (r GenerateResponse) Truncated() bool { return r.DoneReason == "length" }

// Generate sends one prompt and returns the whole reply.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	body := generateBody{
		Model:   c.model,
		Prompt:  req.Prompt,
		Stream:  false,
		Think:   req.Think,
		Options: req.Options,
	}

	var out GenerateResponse
	if err := c.post(ctx, "/api/generate", body, &out); err != nil {
		return GenerateResponse{}, err
	}
	return out, nil
}

// Version reports the Ollama server's version, and doubles as a reachability
// check that costs no GPU time.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.get(ctx, "/api/version", &out); err != nil {
		return "", err
	}
	return out.Version, nil
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("llm: encoding request for %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("llm: building request for %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("llm: building request for %s: %w", path, err)
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// Checked first: if the caller cancelled or ran out of time, that is
		// the reason, whatever the connection reported as it was torn down.
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return fmt.Errorf("llm: %s %s: %w", req.Method, req.URL.Path, ctxErr)
		}
		// Two %w verbs wrap both errors: callers can ask errors.Is about our
		// sentinel and about the network error underneath it.
		if netx.Unreachable(err) {
			return fmt.Errorf("llm: %s %s: %w: %w", req.Method, req.URL.Path, ErrUnavailable, err)
		}
		return fmt.Errorf("llm: %s %s: %w", req.Method, req.URL.Path, err)
	}
	// Close every body, including the ones we never read: an unclosed body
	// holds its connection out of the pool until the GC gets to it.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiError(req, resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		// The context can also end while the body is still arriving.
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return fmt.Errorf("llm: %s %s: %w", req.Method, req.URL.Path, ctxErr)
		}
		return fmt.Errorf("llm: decoding %s reply: %w", req.URL.Path, err)
	}
	return nil
}

// apiError describes a reply that wasn't 200 OK. Ollama reports most failures
// as {"error": "..."}, but a mistyped path is answered by the router in front
// of its handlers in plain text, so the body is only treated as JSON if it
// parses. That difference is also how a missing model is told apart from a
// missing endpoint: both are 404s, and only Ollama's own says why.
func apiError(req *http.Request, resp *http.Response) *APIError {
	e := &APIError{Method: req.Method, Path: req.URL.Path, StatusCode: resp.StatusCode}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return e
	}

	var wire struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &wire) == nil && wire.Error != "" {
		e.Message = wire.Error
		if resp.StatusCode == http.StatusNotFound && strings.Contains(wire.Error, "not found") {
			e.Err = ErrModelNotFound
		}
		return e
	}
	e.Message = string(bytes.TrimSpace(body))
	return e
}

// Bool, Float64 and Int return pointers to their argument, for the option
// fields where zero and unset have to stay distinguishable.
func Bool(v bool) *bool          { return &v }
func Float64(v float64) *float64 { return &v }
func Int(v int) *int             { return &v }

// ModelDetails describes how a pulled model was built.
type ModelDetails struct {
	ParameterSize     string `json:"parameter_size"`
	QuantizationLevel string `json:"quantization_level"`
	ContextLength     int    `json:"context_length"`
}

// Model is one model the server has pulled.
type Model struct {
	Name         string       `json:"name"`
	Size         int64        `json:"size"`
	ModifiedAt   time.Time    `json:"modified_at"`
	Details      ModelDetails `json:"details"`
	Capabilities []string     `json:"capabilities"`
}

// Supports reports whether the model advertises a capability. The ones that
// matter here are "tools", which milestone 5 depends on, and "thinking",
// which decides whether think:false is doing anything.
func (m Model) Supports(capability string) bool {
	return slices.Contains(m.Capabilities, capability)
}

// Models lists what this server has pulled. The agent is developed on one
// machine and runs on another, so "what is actually on that box" has to be a
// question the binary itself can answer.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var out struct {
		Models []Model `json:"models"`
	}
	if err := c.get(ctx, "/api/tags", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}

// RunningModel is a model the server currently holds in memory.
type RunningModel struct {
	Name          string       `json:"name"`
	Size          int64        `json:"size"`
	SizeVRAM      int64        `json:"size_vram"`
	ContextLength int          `json:"context_length"`
	ExpiresAt     time.Time    `json:"expires_at"`
	Details       ModelDetails `json:"details"`
}

// OnGPU reports how much of the loaded model sits in VRAM, from 0 (entirely
// in system RAM) to 1 (entirely on the card). A model too large for the GPU
// keeps running with some layers on the CPU, which is the difference between
// a heavy tier that is merely slow and one that is unusable.
func (r RunningModel) OnGPU() float64 {
	if r.Size == 0 {
		return 0
	}
	return float64(r.SizeVRAM) / float64(r.Size)
}

// Running lists what the server is holding in memory right now.
func (c *Client) Running(ctx context.Context) ([]RunningModel, error) {
	var out struct {
		Models []RunningModel `json:"models"`
	}
	if err := c.get(ctx, "/api/ps", &out); err != nil {
		return nil, err
	}
	return out.Models, nil
}
