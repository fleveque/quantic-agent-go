package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// benchServer answers the four endpoints a benchmark run touches, with
// numbers chosen so the reported rates are exact: 2000 prompt tokens in
// 0.5s is 4000 tok/s, 100 generated tokens in 2s is 50 tok/s.
func benchServer(t *testing.T, onGPU bool) *httptest.Server {
	t.Helper()
	vram := 0
	if onGPU {
		vram = 6222564555
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen3.5:9b","size":6594474711,` +
				`"details":{"parameter_size":"9.7B","quantization_level":"Q4_K_M"},` +
				`"capabilities":["completion","tools"]}]}`))
		case "/api/generate":
			w.Write([]byte(`{"model":"qwen3.5:9b","response":"done","done":true,` +
				`"done_reason":"stop","prompt_eval_count":2000,"prompt_eval_duration":500000000,` +
				`"eval_count":100,"eval_duration":2000000000,"total_duration":2500000000,` +
				`"load_duration":1000000000}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"qwen3.5:9b","size":6222564555,` +
				`"size_vram":` + strconv.Itoa(vram) + `,"context_length":4096}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunTable(t *testing.T) {
	srv := benchServer(t, true)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "qwen3.5:9b", "-contexts", "4096", "-predict", "100"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"qwen3.5:9b", "4K", "4000", "50.0", "100%"} {
		if !strings.Contains(got, want) {
			t.Errorf("table = %q, want it to contain %q", got, want)
		}
	}
}

func TestRunReportsCPUFallback(t *testing.T) {
	srv := benchServer(t, false)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "qwen3.5:9b", "-contexts", "4096"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	// A model that isn't on the GPU is the single most important thing this
	// tool can tell you, so it must be unmissable.
	if got := stdout.String(); !strings.Contains(got, "0% (CPU)") {
		t.Errorf("table = %q, want it to flag CPU execution", got)
	}
}

func TestRunJSON(t *testing.T) {
	srv := benchServer(t, true)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "qwen3.5:9b", "-contexts", "4096", "-json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}

	var results []result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decoding output: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	got := results[0]
	if got.PromptRate != 4000 {
		t.Errorf("PromptRate = %v, want 4000", got.PromptRate)
	}
	if got.GenRate != 50 {
		t.Errorf("GenRate = %v, want 50", got.GenRate)
	}
	if got.OnGPU != 1 {
		t.Errorf("OnGPU = %v, want 1", got.OnGPU)
	}
	if got.LoadSeconds != 1 {
		t.Errorf("LoadSeconds = %v, want 1", got.LoadSeconds)
	}
}

func TestRunSkipsModelsThatArentPulled(t *testing.T) {
	srv := benchServer(t, true)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "not-pulled:latest", "-contexts", "4096"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "not-pulled:latest is not on this server") {
		t.Errorf("stderr = %q, want it to name the missing model", got)
	}
}

func TestParseSizes(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []int
		wantErr bool
	}{
		{name: "one", in: "4096", want: []int{4096}},
		{name: "several with spaces", in: "4096, 32768", want: []int{4096, 32768}},
		{name: "not a number", in: "4096,big", wantErr: true},
		{name: "zero", in: "0", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSizes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseSizes(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSizes(%q): %v", tt.in, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseSizes(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("parseSizes(%q)[%d] = %d, want %d", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// Ollama resolves model names case-insensitively, so a hand-typed
// hf.co/... name in the wrong case must still be found and measured.
func TestRunMatchesModelNamesCaseInsensitively(t *testing.T) {
	srv := benchServer(t, true)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "QWEN3.5:9B", "-contexts", "4096", "-predict", "100"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	// Residency is matched by name as well, so 100% proves both lookups.
	if got := stdout.String(); !strings.Contains(got, "100%") {
		t.Errorf("table = %q, want the model found and shown fully on GPU", got)
	}
}

func TestRunFlagsAShortGenerationSample(t *testing.T) {
	srv := benchServer(t, true) // the fake server always generates 100 tokens

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "qwen3.5:9b", "-contexts", "4096", "-predict", "128"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "stopped after 100 of 128 tokens") {
		t.Errorf("table = %q, want the short sample flagged", got)
	}
}

// If the server goes away mid-run, the benchmark stops asking and reports
// what it had already measured, instead of failing every remaining request.
func TestRunStopsWhenTheServerGoesAway(t *testing.T) {
	// The handler runs on the server's goroutines, so shared state needs a lock.
	var (
		mu        sync.Mutex
		generates int
		asked     []string // the model named in each generate request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"first:latest"},{"name":"second:latest"}]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[]}`))
		case "/api/generate":
			var body struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			mu.Lock()
			generates++
			n := generates
			asked = append(asked, body.Model)
			mu.Unlock()

			// Warm-up and the first measurement succeed. From the third
			// request on, the connection is dropped unanswered, the way a
			// restarting Ollama drops it.
			if n >= 3 {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Errorf("hijacking the connection: %v", err)
					return
				}
				conn.Close()
				return
			}
			w.Write([]byte(`{"response":"done","done":true,"prompt_eval_count":10,` +
				`"prompt_eval_duration":1000000,"eval_count":10,"eval_duration":1000000}`))
		}
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "first:latest,second:latest",
		"-contexts", "4096,8192", "-json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 with a partial result (stderr: %q)", code, stderr.String())
	}
	var results []result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decoding output: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want the 1 measured before the server went away", len(results))
	}
	// Not an exact request count: when a connection drops, Go's HTTP
	// transport sometimes resends the request once by itself. What matters
	// is that the second model is never started.
	mu.Lock()
	defer mu.Unlock()
	for _, m := range asked {
		if m == "second:latest" {
			t.Errorf("the second model was requested after the server went away (requests: %v)", asked)
			break
		}
	}
	if got := stderr.String(); !strings.Contains(got, "model server unavailable") {
		t.Errorf("stderr = %q, want it to report the server as unavailable", got)
	}
}

// twoModelServer serves two models. Generate requests are passed to answer,
// which reports whether to reply normally; if not, the handler waits for the
// client to hang up, as a busy server would. It records the model of every
// generate request.
func twoModelServer(t *testing.T, answer func(model string, n int) bool) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"first:latest"},{"name":"second:latest"}]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[]}`))
		case "/api/generate":
			var body struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&body) // also lets the server notice a hang-up

			mu.Lock()
			asked = append(asked, body.Model)
			n := len(asked)
			mu.Unlock()

			if !answer(body.Model, n) {
				<-r.Context().Done()
				return
			}
			w.Write([]byte(`{"response":"done","done":true,"prompt_eval_count":10,` +
				`"prompt_eval_duration":1000000,"eval_count":10,"eval_duration":1000000}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// Ctrl-C mid-run: report what was measured, exit 130, start nothing new.
func TestRunStopsWhenInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	srv, asked := twoModelServer(t, func(model string, n int) bool {
		if n >= 3 { // warm-up and the first measurement succeed
			cancel() // what Ctrl-C does to the context main hands to run
			return false
		}
		return true
	})

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-ollama", srv.URL, "-models", "first:latest,second:latest",
		"-contexts", "4096,8192", "-json"}, &stdout, &stderr)

	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	var results []result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decoding output: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 {
		t.Errorf("got %d results, want the 1 measured before the interrupt", len(results))
	}
	if slices.Contains(asked(), "second:latest") {
		t.Errorf("the second model was requested after the interrupt (requests: %v)", asked())
	}
}

// One request running out of time is that request's problem, not the run's:
// the next model is still measured.
func TestRunMovesOnAfterATimeout(t *testing.T) {
	srv, _ := twoModelServer(t, func(model string, n int) bool {
		return model != "first:latest" // the first model never answers
	})

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-models", "first:latest,second:latest",
		"-contexts", "4096", "-timeout", "50ms", "-json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	var results []result
	if err := json.Unmarshal(stdout.Bytes(), &results); err != nil {
		t.Fatalf("decoding output: %v\n%s", err, stdout.String())
	}
	if len(results) != 1 || results[0].Model != "second:latest" {
		t.Errorf("results = %+v, want one measurement of second:latest", results)
	}
	if got := stderr.String(); !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("stderr = %q, want the first model's timeout reported", got)
	}
}
