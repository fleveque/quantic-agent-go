package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain runs before any test in the package. It points the default
// database at a temporary directory, so no test can ever write to the
// operator's real run history. (An early version of these tests did: the
// research tests didn't pass -db, and left three fake runs in
// ~/.local/state/quantic-agent.)
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "quantic-agent-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	os.Unsetenv("QUANTIC_AGENT_DB")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string // substring; empty means stderr must be empty
	}{
		{
			name:       "no arguments",
			args:       nil,
			wantCode:   0,
			wantStdout: "quantic-agent: no tasks defined yet\n",
		},
		{
			name:       "version",
			args:       []string{"-version"},
			wantCode:   0,
			wantStdout: "quantic-agent dev\n",
		},
		{
			name:       "help",
			args:       []string{"-h"},
			wantCode:   0,
			wantStderr: "-version",
		},
		{
			name:       "unknown flag",
			args:       []string{"-publish"},
			wantCode:   2,
			wantStderr: "flag provided but not defined: -publish",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := run(t.Context(), tt.args, &stdout, &stderr)

			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if got := stdout.String(); got != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tt.wantStdout)
			}
			if tt.wantStderr == "" && stderr.Len() > 0 {
				t.Errorf("stderr = %q, want it empty", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestRunAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"quantic-9b:latest","response":"ok","done":true,` +
			`"done_reason":"stop","eval_count":2,"eval_duration":205098000}`))
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-ask", "Reply with exactly: ok"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if got := stdout.String(); got != "ok\n" {
		t.Errorf("stdout = %q, want %q", got, "ok\n")
	}
	// The run line goes to stderr so that stdout stays pipeable.
	if got := stderr.String(); !strings.Contains(got, "2 tokens") {
		t.Errorf("stderr = %q, want it to report the token count", got)
	}
}

// checkServer answers the two endpoints -check calls.
func checkServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.30.3"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen3.5:9b","size":6594474711,` +
				`"details":{"parameter_size":"9.7B","quantization_level":"Q4_K_M",` +
				`"context_length":262144},"capabilities":["completion","tools","thinking"]}]}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunCheck(t *testing.T) {
	srv := checkServer(t)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-model", "qwen3.5:9b", "-check"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	got := stdout.String()
	for _, want := range []string{"ollama 0.30.3", "* qwen3.5:9b", "6.6 GB", "256K", "tools"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout = %q, want it to contain %q", got, want)
		}
	}
}

func TestRunCheckFlagsAMissingModel(t *testing.T) {
	srv := checkServer(t)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-model", "not-pulled:latest", "-check"}, &stdout, &stderr)

	// A model that was never pulled on the target machine has to fail here,
	// not later in the middle of a task.
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "not-pulled:latest is not on this server") {
		t.Errorf("stderr = %q, want it to name the missing model", got)
	}
}

func TestRunReportsAnUnreachableServer(t *testing.T) {
	// Nothing listens on port 1, as with a stopped Ollama. (A closed test
	// server's port is not safe: a test binary running in parallel can be
	// given it a moment later, and then the "dead" server answers.)
	const deadURL = "http://127.0.0.1:1"

	for _, args := range [][]string{
		{"-ollama", deadURL, "-ask", "hi"},
		{"-ollama", deadURL, "-check"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(t.Context(), args, &stdout, &stderr)

		// 3, not 1: nothing was attempted, so a scheduler can retry the run.
		if code != 3 {
			t.Errorf("%v: exit code = %d, want 3", args, code)
		}
		if stdout.Len() > 0 {
			t.Errorf("%v: stdout = %q, want it empty on failure", args, stdout.String())
		}
		if got := stderr.String(); !strings.Contains(got, "Is Ollama running?") {
			t.Errorf("%v: stderr = %q, want it to suggest checking the server", args, got)
		}
	}
}

func TestRunAskReportsAMissingModel(t *testing.T) {
	// What Ollama 0.34.4 answers for a model it doesn't have.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model 'not-pulled:latest' not found"}`))
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-model", "not-pulled:latest", "-ask", "hi"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "ollama pull not-pulled:latest") {
		t.Errorf("stderr = %q, want it to say how to pull the model", got)
	}
}

func TestRunPointsAtTheServerLogOnAServerFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-ask", "hi"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	got := stderr.String()
	if !strings.Contains(got, "500 Internal Server Error") {
		t.Errorf("stderr = %q, want the status", got)
	}
	if !strings.Contains(got, "journalctl -u ollama") {
		t.Errorf("stderr = %q, want it to point at the server's log", got)
	}
}

// Ollama resolves model names case-insensitively, so -check must too, or it
// reports a perfectly usable model as missing.
func TestRunCheckMatchesModelNamesCaseInsensitively(t *testing.T) {
	srv := checkServer(t)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-model", "QWEN3.5:9B", "-check"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "* qwen3.5:9b") {
		t.Errorf("stdout = %q, want the model marked as selected", got)
	}
}

// slowServer reads each request and then never answers, like a server busy
// loading a model. Each handler returns once its client hangs up.
func slowServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // until the body is read, a hang-up goes unnoticed
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunGivesUpAtTheTimeout(t *testing.T) {
	srv := slowServer(t)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", srv.URL, "-timeout", "50ms", "-ask", "hi"}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got := stderr.String(); !strings.Contains(got, "gave up after 50ms") {
		t.Errorf("stderr = %q, want it to say how long it waited", got)
	}
}

func TestRunStopsWhenInterrupted(t *testing.T) {
	srv := slowServer(t)
	// The context main gets from signal.NotifyContext, as it is after Ctrl-C.
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-ollama", srv.URL, "-ask", "hi"}, &stdout, &stderr)

	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	if got := stderr.String(); !strings.Contains(got, "stopped") {
		t.Errorf("stderr = %q, want it to say the run was stopped", got)
	}
}

// fakeMCP is a minimal MCP server: it completes the handshake and answers
// dividend_calendar with calendar, a tool result as JSON text.
func fakeMCP(t *testing.T, calendar string) *httptest.Server {
	t.Helper()
	text, _ := json.Marshal(calendar) // the result travels as a JSON string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "application/json")
		switch msg.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"fake"}}}`, msg.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%s}],"isError":false}}`, msg.ID, text)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOllamaChat replays the real two-turn exchange captured in
// internal/llm/testdata: first the tool request, then the answer.
func fakeOllamaChat(t *testing.T) *httptest.Server {
	t.Helper()
	var turn atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		file := "chat-tool-call.json"
		if turn.Add(1) > 1 {
			file = "chat-tool-answer.json"
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "internal", "llm", "testdata", file))
		if err != nil {
			t.Errorf("fixture: %v", err)
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// realCalendar is the 10-day calendar the captured answer was written from.
func realCalendar(t *testing.T) string {
	t.Helper()
	sse, err := os.ReadFile(filepath.Join("..", "..", "internal", "mcp", "testdata", "call-dividend-calendar.sse"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(sse), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var msg struct {
				Result struct {
					Content []struct{ Text string } `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				t.Fatal(err)
			}
			return msg.Result.Content[0].Text
		}
	}
	t.Fatal("no data line")
	return ""
}

func TestRunResearch(t *testing.T) {
	ollama, quantic := fakeOllamaChat(t), fakeMCP(t, realCalendar(t))

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", ollama.URL, "-mcp", quantic.URL,
		"-research", "Which companies go ex-dividend in the next 10 days?"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Microsoft") {
		t.Errorf("stdout = %q, want the model's answer", stdout.String())
	}
	// Every tool call is traced on stderr, so a person can see what the
	// answer was built from.
	if got := stderr.String(); !strings.Contains(got, `tool dividend_calendar {"days":10} → `) {
		t.Errorf("stderr = %q, want the tool call traced", got)
	}
}

func TestRunResearchWithoutAnMCPServer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-mcp", "http://127.0.0.1:1/mcp", "-research", "q"}, &stdout, &stderr)

	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if got := stderr.String(); !strings.Contains(got, "no MCP server answering at http://127.0.0.1:1/mcp") {
		t.Errorf("stderr = %q, want it to name the MCP server", got)
	}
}

// The same real answer, but the tool returned an empty calendar: every date in
// the answer is now unaccounted for, and the run says so.
func TestRunResearchRejectsFiguresNoToolReturned(t *testing.T) {
	ollama, quantic := fakeOllamaChat(t), fakeMCP(t, `{"from":"2026-10-06","days":10,"stocks":[]}`)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-ollama", ollama.URL, "-mcp", quantic.URL, "-research", "q"}, &stdout, &stderr)

	if code != 4 {
		t.Errorf("exit code = %d, want 4", code)
	}
	if stdout.Len() == 0 {
		t.Error("stdout is empty; the answer should still be shown")
	}
	got := stderr.String()
	for _, want := range []string{"came from no tool result", `"Oct 8" (date --10-08)`, `"Oct 16"`} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want it to mention %q", got, want)
		}
	}
	// Dates the empty calendar does contain are still accounted for.
	if strings.Contains(got, "October 6, 2026") {
		t.Errorf("stderr = %q; 2026-10-06 is the calendar's own start date", got)
	}
}

// A research run is stored as it happens; -runs lists it and -run re-checks
// its answer against the stored tool results.
func TestResearchIsRecordedAndCanBeRechecked(t *testing.T) {
	ollama, quantic := fakeOllamaChat(t), fakeMCP(t, realCalendar(t))
	db := filepath.Join(t.TempDir(), "agent.db")

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-db", db, "-ollama", ollama.URL, "-mcp", quantic.URL,
		"-research", "Which companies go ex-dividend in the next 10 days?"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("research exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent: run 1 answered") {
		t.Errorf("stderr = %q, want the run's id and outcome", stderr.String())
	}

	stdout.Reset()
	if code := run(t.Context(), []string{"-db", db, "-runs"}, &stdout, &stderr); code != 0 {
		t.Fatalf("-runs exit %d", code)
	}
	if got := stdout.String(); !strings.Contains(got, "answered") || !strings.Contains(got, "Which companies go ex-dividend") {
		t.Errorf("-runs = %q, want the run listed", got)
	}

	stdout.Reset()
	if code := run(t.Context(), []string{"-db", db, "-run", "1"}, &stdout, &stderr); code != 0 {
		t.Fatalf("-run 1 exit %d: %s", code, stdout.String())
	}
	got := stdout.String()
	for _, want := range []string{`call 0: dividend_calendar {"days":10}`, "Microsoft", "every figure traces to a stored tool result"} {
		if !strings.Contains(got, want) {
			t.Errorf("-run 1 = %q, want it to contain %q", got, want)
		}
	}
}

// Ctrl-C mid-run: the context is cancelled, and the outcome must still be
// saved, which is exactly the moment a cancelled context can't be used for it.
func TestAnInterruptedRunIsRecorded(t *testing.T) {
	quantic := fakeMCP(t, realCalendar(t))
	ollama := slowServer(t)
	db := filepath.Join(t.TempDir(), "agent.db")
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)

	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-db", db, "-ollama", ollama.URL, "-mcp", quantic.URL, "-research", "q"}, &stdout, &stderr)

	if code != 130 {
		t.Errorf("exit code = %d, want 130", code)
	}
	stdout.Reset()
	run(t.Context(), []string{"-db", db, "-run", "1"}, &stdout, &stderr)
	if got := stdout.String(); !strings.Contains(got, "run 1 · interrupted") || !strings.Contains(got, "context canceled") {
		t.Errorf("-run 1 = %q, want the run saved as interrupted", got)
	}
}
