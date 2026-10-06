package mcp_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fleveque/quantic-agent/internal/mcp"
)

// The files under testdata/ are replies captured from quantic.finance/mcp,
// anonymously, on 2026-10-06. They contain public reference data only.

// reply loads a captured reply and gives it the id of the request being
// answered: the captures came from a session whose request ids differ from
// the ones this client will use. A .sse capture is re-sent as an event
// stream and a .json one as a plain body, so both transports are exercised.
func reply(t *testing.T, w http.ResponseWriter, file string, id any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	payload := raw
	if strings.HasSuffix(file, ".sse") {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				payload = []byte(data)
			}
		}
	}
	var msg map[string]any
	if err := json.Unmarshal(payload, &msg); err != nil {
		t.Fatalf("fixture %s: %v", file, err)
	}
	msg["id"] = id
	out, _ := json.Marshal(msg)

	if strings.HasSuffix(file, ".sse") {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprintf(w, "id: 0\nevent: message\ndata: %s\n\n", out)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(out)
}

// request is what the fake server saw: the JSON-RPC message and the headers
// that matter.
type request struct {
	Method        string
	ID            any
	Params        map[string]any
	SessionID     string
	Authorization string
}

// fakeServer answers like Quantic's: a session id on initialize, 202 for the
// notification, and the captured reply for each tool. answers maps a method
// (or "tools/call:<name>") to a fixture file.
func fakeServer(t *testing.T, answers map[string]string) (*httptest.Server, func() []request) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []request
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			t.Errorf("decoding request: %v", err)
			return
		}
		mu.Lock()
		seen = append(seen, request{msg.Method, msg.ID, msg.Params, r.Header.Get("Mcp-Session-Id"), r.Header.Get("Authorization")})
		mu.Unlock()

		key := msg.Method
		if msg.Method == "tools/call" {
			key += ":" + fmt.Sprint(msg.Params["name"])
		}
		switch {
		case msg.Method == "initialize":
			w.Header().Set("Mcp-Session-Id", "session_test")
			reply(t, w, "initialize.sse", msg.ID)
		case msg.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case answers[key] != "":
			reply(t, w, answers[key], msg.ID)
		default:
			t.Errorf("unexpected request %q", key)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []request {
		mu.Lock()
		defer mu.Unlock()
		return append([]request(nil), seen...)
	}
}

func TestInitializeAndCallTool(t *testing.T) {
	srv, seen := fakeServer(t, map[string]string{"tools/call:dividend_calendar": "call-dividend-calendar.sse"})
	c := mcp.New(srv.URL, "")

	info, err := c.Initialize(t.Context())
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if info.Name != "Quantic Finance" {
		t.Errorf("server name = %q, want Quantic Finance", info.Name)
	}

	res, err := c.CallTool(t.Context(), "dividend_calendar", map[string]any{"days": 10})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("IsError = true, want false (text: %s)", res.Text)
	}

	// The tool's output is itself JSON, inside the JSON-RPC message.
	var calendar struct {
		Days   int `json:"days"`
		Stocks []struct {
			Symbol         string `json:"symbol"`
			ExDividendDate string `json:"ex_dividend_date"`
		} `json:"stocks"`
	}
	if err := json.Unmarshal([]byte(res.Text), &calendar); err != nil {
		t.Fatalf("result text is not the calendar JSON: %v\n%s", err, res.Text)
	}
	if calendar.Days != 10 || len(calendar.Stocks) == 0 || calendar.Stocks[0].Symbol != "O" {
		t.Errorf("calendar = %+v, want the captured 10-day calendar starting with O", calendar)
	}

	reqs := seen()
	if len(reqs) != 3 {
		t.Fatalf("server saw %d requests, want initialize, initialized, tools/call", len(reqs))
	}
	// The session the server opened is sent back on every later request.
	if reqs[0].SessionID != "" || reqs[1].SessionID != "session_test" || reqs[2].SessionID != "session_test" {
		t.Errorf("session ids sent = %q, %q, %q; want none, then session_test", reqs[0].SessionID, reqs[1].SessionID, reqs[2].SessionID)
	}
	// A notification has no id, so it gets no reply.
	if reqs[1].ID != nil {
		t.Errorf("notifications/initialized carried id %v, want none", reqs[1].ID)
	}
	// Anonymous: no credentials at all.
	for _, r := range reqs {
		if r.Authorization != "" {
			t.Errorf("%s sent Authorization %q, want none for an anonymous client", r.Method, r.Authorization)
		}
	}
}

func TestListToolsFromAJSONReply(t *testing.T) {
	srv, _ := fakeServer(t, map[string]string{"tools/list": "tools-list.json"})
	c := mcp.New(srv.URL, "")

	tools, err := c.ListTools(t.Context())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found bool
	for _, tool := range tools {
		if tool.Name == "dividend_calendar" {
			found = true
			if !strings.Contains(string(tool.InputSchema), `"days"`) {
				t.Errorf("dividend_calendar schema = %s, want a days property", tool.InputSchema)
			}
		}
	}
	if !found {
		t.Errorf("dividend_calendar not among %d tools", len(tools))
	}
}

func TestToolRefusalIsAResultNotAnError(t *testing.T) {
	// A portfolio tool called without an account: the tool runs and refuses.
	srv, _ := fakeServer(t, map[string]string{"tools/call:get_holdings": "call-private-anonymous.json"})

	res, err := mcp.New(srv.URL, "").CallTool(t.Context(), "get_holdings", map[string]any{})

	if err != nil {
		t.Fatalf("CallTool: %v, want the refusal as a result", err)
	}
	if !res.IsError {
		t.Error("IsError = false, want true")
	}
	if !strings.Contains(res.Text, "needs authentication") {
		t.Errorf("Text = %q, want the server's explanation", res.Text)
	}
}

func TestProtocolErrors(t *testing.T) {
	tests := []struct {
		tool, file, wantData string
	}{
		{"no_such_tool", "error-unknown-tool.json", "Tool not found: no_such_tool"},
		{"dividend_calendar", "error-bad-arguments.json", `days: expected type of :integer received "ten" value`},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			srv, _ := fakeServer(t, map[string]string{"tools/call:" + tt.tool: tt.file})

			_, err := mcp.New(srv.URL, "").CallTool(t.Context(), tt.tool, map[string]any{})

			var rpcErr *mcp.RPCError
			if !errors.As(err, &rpcErr) {
				t.Fatalf("error %v is not an *mcp.RPCError", err)
			}
			if rpcErr.Code != -32602 || rpcErr.Data.Message != tt.wantData {
				t.Errorf("RPCError = %d %q, want -32602 %q", rpcErr.Code, rpcErr.Data.Message, tt.wantData)
			}
		})
	}
}

func TestTokenIsSentAndARejectionIsUnauthorized(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		io.Copy(io.Discard, r.Body)
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	_, err := mcp.New(srv.URL, "qtc_example").ListTools(t.Context())

	if got != "Bearer qtc_example" {
		t.Errorf("Authorization = %q, want Bearer qtc_example", got)
	}
	if !errors.Is(err, mcp.ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false for %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	// Nothing listens on port 1 (see internal/llm for why not a closed test server).
	_, err := mcp.New("http://127.0.0.1:1/mcp", "").Initialize(t.Context())

	if !errors.Is(err, mcp.ErrUnavailable) {
		t.Errorf("errors.Is(err, ErrUnavailable) = false for %v", err)
	}
}

func TestEventStreamSkipsOtherMessages(t *testing.T) {
	// A server may send notifications on the stream before the reply.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID int64 `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": a comment\n\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\n", msg.ID)
		fmt.Fprint(w, "data: \"result\":{\"tools\":[{\"name\":\"t\"}]}}\n\n")
	}))
	t.Cleanup(srv.Close)

	tools, err := mcp.New(srv.URL, "").ListTools(t.Context())

	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "t" {
		t.Errorf("tools = %+v, want the one in the reply (split across two data lines)", tools)
	}
}

func TestEventStreamWithoutAReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
	}))
	t.Cleanup(srv.Close)

	_, err := mcp.New(srv.URL, "").ListTools(t.Context())

	if err == nil || !strings.Contains(err.Error(), "ended without a reply") {
		t.Errorf("err = %v, want the stream ending without a reply reported", err)
	}
}
