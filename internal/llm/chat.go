package llm

import (
	"context"
	"encoding/json"
	"time"
)

// Chat is a conversation turn through /api/chat: the messages so far go in,
// the model's next message comes out. Unlike Generate it can offer the model
// tools, and the model's reply may then be a request to call one instead of
// an answer.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body := chatBody{
		Model:    c.model,
		Messages: req.Messages,
		Tools:    req.Tools,
		Stream:   false, // see generateBody: it must be sent
		Think:    req.Think,
		Options:  req.Options,
	}

	var out ChatResponse
	if err := c.post(ctx, "/api/chat", body, &out); err != nil {
		return ChatResponse{}, err
	}
	return out, nil
}

// ChatRequest is the part of a chat turn the caller varies.
type ChatRequest struct {
	Messages []Message
	Tools    []ToolDef
	Think    *bool
	Options  *Options
}

// Message is one message in a conversation. Role is "system", "user",
// "assistant" or "tool".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`

	// ToolCalls is set on an assistant message that asks for tools to be
	// called instead of answering. Sending the message back as part of the
	// history keeps the conversation coherent for the model.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolName is set on a "tool" message: which tool's output Content is.
	ToolName string `json:"tool_name,omitempty"`
}

// ToolCall is one tool the model asked for, with the arguments it chose.
type ToolCall struct {
	ID       string       `json:"id,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names a tool and carries its arguments.
//
// Arguments stay as raw JSON: whether they fit the tool is for the tool to
// decide (tools.Tool.DecodeArgs), and a model can produce anything.
// json.RawMessage holds the bytes exactly as they arrived, and encodes them
// back unchanged when the message is sent again.
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolDef offers one tool to the model. Type is always "function".
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is what the model is told about a tool. Parameters is a JSON
// Schema object; any type that encodes to one works (tools.Schema does).
type FunctionDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
}

// chatBody is the wire format of POST /api/chat.
type chatBody struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
	Think    *bool     `json:"think,omitempty"`
	Options  *Options  `json:"options,omitempty"`
}

// ChatResponse is one non-streamed reply from /api/chat.
type ChatResponse struct {
	Model      string    `json:"model"`
	CreatedAt  time.Time `json:"created_at"`
	Message    Message   `json:"message"`
	Done       bool      `json:"done"`
	DoneReason string    `json:"done_reason"`

	PromptEvalCount int `json:"prompt_eval_count"`
	EvalCount       int `json:"eval_count"`

	TotalDuration      time.Duration `json:"total_duration"`
	LoadDuration       time.Duration `json:"load_duration"`
	PromptEvalDuration time.Duration `json:"prompt_eval_duration"`
	EvalDuration       time.Duration `json:"eval_duration"`
}

// Truncated reports whether the model ran out of token budget mid-reply.
func (r ChatResponse) Truncated() bool { return r.DoneReason == "length" }
