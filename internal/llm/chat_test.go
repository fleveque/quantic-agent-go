package llm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fleveque/quantic-agent/internal/llm"
)

// The two chat fixtures are one real exchange with qwen3.5:9b on Ollama
// 0.34.4: offered dividend_calendar, the model asked for it with days 10
// (chat-tool-call.json); given the tool's real output, it answered
// (chat-tool-answer.json).

var calendarTool = llm.ToolDef{Type: "function", Function: llm.FunctionDef{
	Name:        "dividend_calendar",
	Description: "List upcoming ex-dividend dates.",
	Parameters:  map[string]any{"type": "object", "properties": map[string]any{"days": map[string]string{"type": "integer"}}},
}}

func TestChatDecodesAToolCall(t *testing.T) {
	var sent map[string]any
	srv := serveFixture(t, "chat-tool-call.json", &sent)

	resp, err := llm.New(srv.URL, "qwen3.5:9b").Chat(t.Context(), llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "Which companies go ex-dividend in the next 10 days?"}},
		Tools:    []llm.ToolDef{calendarTool},
		Think:    llm.Bool(false),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	calls := resp.Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1 (message: %+v)", len(calls), resp.Message)
	}
	if calls[0].Function.Name != "dividend_calendar" {
		t.Errorf("tool = %q, want dividend_calendar", calls[0].Function.Name)
	}
	// The arguments arrive as a JSON object, kept byte for byte.
	if got := string(calls[0].Function.Arguments); got != `{"days":10}` {
		t.Errorf("arguments = %s, want {\"days\":10}", got)
	}
	if resp.Message.Content != "" {
		t.Errorf("content = %q, want it empty when the model asks for a tool", resp.Message.Content)
	}

	// The request carried the tools, and the two settings that must be explicit.
	if tools, ok := sent["tools"].([]any); !ok || len(tools) != 1 {
		t.Errorf("request tools = %v, want the one tool", sent["tools"])
	}
	if sent["stream"] != false || sent["think"] != false {
		t.Errorf("request stream = %v, think = %v; want both false", sent["stream"], sent["think"])
	}
}

func TestChatSendsTheToolResultBack(t *testing.T) {
	var sent map[string]any
	srv := serveFixture(t, "chat-tool-answer.json", &sent)

	// The history as the research loop builds it: the question, the model's
	// tool call sent back unchanged, then the tool's output.
	call := llm.ToolCall{ID: "call_1", Function: llm.FunctionCall{Name: "dividend_calendar", Arguments: json.RawMessage(`{"days":10}`)}}
	resp, err := llm.New(srv.URL, "qwen3.5:9b").Chat(t.Context(), llm.ChatRequest{
		Messages: []llm.Message{
			{Role: "user", Content: "Which companies go ex-dividend in the next 10 days?"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
			{Role: "tool", ToolName: "dividend_calendar", Content: `{"days":10,"stocks":[]}`},
		},
		Tools: []llm.ToolDef{calendarTool},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(resp.Message.Content, "Microsoft") || len(resp.Message.ToolCalls) != 0 {
		t.Errorf("message = %+v, want the captured answer and no further tool calls", resp.Message)
	}

	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("request carried %d messages, want 3", len(msgs))
	}
	assistant := msgs[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	args := calls[0].(map[string]any)["function"].(map[string]any)["arguments"]
	// Sent back as an object, not as a string containing JSON.
	if m, ok := args.(map[string]any); !ok || m["days"] != float64(10) {
		t.Errorf("tool call arguments sent as %#v, want the object {days: 10}", args)
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_name"] != "dividend_calendar" {
		t.Errorf("tool message = %v, want role tool and tool_name dividend_calendar", tool)
	}
	// Fields that don't apply are left out, not sent empty.
	if _, ok := tool["tool_calls"]; ok {
		t.Errorf("tool message carries tool_calls: %v", tool)
	}
}
