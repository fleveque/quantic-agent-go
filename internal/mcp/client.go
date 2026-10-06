// Package mcp is the client for Quantic's MCP server, the only source of
// financial facts the agent is allowed (design N1).
//
// MCP is JSON-RPC 2.0 sent over HTTP ("Streamable HTTP" transport): every
// request is a POST of one JSON-RPC message, and the reply comes back either
// as a JSON body or as a short Server-Sent Events stream carrying the same
// message. The server decides which; a client has to accept both. A session
// starts with an initialize request, whose reply carries an Mcp-Session-Id
// header that later requests send back.
//
// The agent connects anonymously by default. Quantic's public reference tools
// (dividend_calendar, get_stock, screen_stocks, ...) answer anonymous callers,
// rate limited; the tools that read a user's portfolio refuse them. An
// anonymous agent therefore cannot reach anyone's data, which enforces design
// N4 on the server rather than trusting the agent to behave
// (decision 0006). A token can be supplied for tools that need one.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fleveque/quantic-agent/internal/netx"
)

// DefaultURL is Quantic's production MCP endpoint.
const DefaultURL = "https://quantic.finance/mcp"

// ProtocolVersion is the MCP revision this client speaks.
const ProtocolVersion = "2025-06-18"

// maxMessage bounds one JSON-RPC message read from the server. A 120-day
// calendar is tens of kilobytes; this leaves room without trusting the server
// to bound itself.
const maxMessage = 4 << 20

var (
	// ErrUnavailable means no reply came back because the server wasn't
	// there to give one (see netx.Unreachable).
	ErrUnavailable = errors.New("mcp server unavailable")

	// ErrUnauthorized means the server rejected the credentials: a token was
	// sent and it isn't valid. Anonymous requests never get this.
	ErrUnauthorized = errors.New("mcp server rejected the credentials")
)

// RPCError is a JSON-RPC error object: the server understood the request and
// refused it at the protocol level, such as an unknown tool (-32602) or an
// argument of the wrong type. It describes a mistake in the request, so the
// research loop hands it back to the model to correct.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Message string `json:"message"`
	} `json:"data"`
}

func (e *RPCError) Error() string {
	if e.Data.Message != "" {
		return fmt.Sprintf("mcp: %s (%d): %s", e.Message, e.Code, e.Data.Message)
	}
	return fmt.Sprintf("mcp: %s (%d)", e.Message, e.Code)
}

// Client is a session with one MCP server. It is safe for concurrent use.
type Client struct {
	url   string
	token string
	http  *http.Client

	nextID atomic.Int64

	mu        sync.Mutex
	sessionID string
}

// New returns a client for the server at url. An empty token connects
// anonymously.
func New(url, token string) *Client {
	return &Client{url: url, token: token, http: &http.Client{}}
}

// ServerInfo is what the server says about itself in the handshake.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Initialize performs the MCP handshake: it announces the client, records
// the session the server opens, and confirms with notifications/initialized.
// Call it once before anything else.
func (c *Client) Initialize(ctx context.Context) (ServerInfo, error) {
	var out struct {
		ProtocolVersion string     `json:"protocolVersion"`
		ServerInfo      ServerInfo `json:"serverInfo"`
	}
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "quantic-agent", "version": "dev"},
	}
	if err := c.call(ctx, "initialize", params, &out); err != nil {
		return ServerInfo{}, err
	}
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		return ServerInfo{}, err
	}
	return out.ServerInfo, nil
}

// Tool describes one tool the server offers.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ListTools returns the tools the server offers.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", nil, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Result is what a tool returned. Text is the tool's output, which for
// Quantic's tools is itself a JSON document. IsError means the tool ran and
// refused (for example a private tool called anonymously); Text then
// explains why. Neither case is a Go error: both are answers to hand back to
// the model.
type Result struct {
	Text    string
	IsError bool
}

// CallTool calls one tool with arguments, which are encoded as JSON.
func (c *Client) CallTool(ctx context.Context, name string, arguments any) (Result, error) {
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	params := map[string]any{"name": name, "arguments": arguments}
	if err := c.call(ctx, "tools/call", params, &out); err != nil {
		return Result{}, err
	}

	// MCP results are a list of content blocks; Quantic's tools return one
	// text block. Concatenating the text blocks keeps that exact and copes
	// with more.
	var text strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return Result{Text: text.String(), IsError: out.IsError}, nil
}

// message is one JSON-RPC 2.0 message, in either direction.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"` // absent on notifications
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// call sends a request and decodes its result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	reply, err := c.send(ctx, message{JSONRPC: "2.0", ID: &id, Method: method, Params: params}, id)
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return reply.Error
	}
	if err := json.Unmarshal(reply.Result, out); err != nil {
		return fmt.Errorf("mcp: decoding %s result: %w", method, err)
	}
	return nil
}

// notify sends a notification: a message with no id, which gets no reply.
func (c *Client) notify(ctx context.Context, method string) error {
	_, err := c.send(ctx, message{JSONRPC: "2.0", Method: method}, 0)
	return err
}

// send POSTs one message and, for a request, returns the reply whose id
// matches wantID. For a notification (wantID 0) it returns an empty message.
func (c *Client) send(ctx context.Context, msg message, wantID int64) (message, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return message{}, fmt.Errorf("mcp: encoding %s: %w", msg.Method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return message{}, fmt.Errorf("mcp: building %s: %w", msg.Method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.mu.Lock()
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
		req.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	}
	c.mu.Unlock()

	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return message{}, fmt.Errorf("mcp: %s: %w", msg.Method, ctxErr)
		}
		if netx.Unreachable(err) {
			return message{}, fmt.Errorf("mcp: %s: %w: %w", msg.Method, ErrUnavailable, err)
		}
		return message{}, fmt.Errorf("mcp: %s: %w", msg.Method, err)
	}
	defer resp.Body.Close()

	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.sessionID = id
		c.mu.Unlock()
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return message{}, fmt.Errorf("mcp: %s: %w", msg.Method, ErrUnauthorized)
	case wantID == 0 && resp.StatusCode == http.StatusAccepted:
		return message{}, nil // a notification, acknowledged
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return message{}, fmt.Errorf("mcp: %s: %s: %s", msg.Method, resp.Status, bytes.TrimSpace(body))
	}

	reply, err := readReply(resp, wantID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return message{}, fmt.Errorf("mcp: %s: %w", msg.Method, ctxErr)
		}
		return message{}, fmt.Errorf("mcp: %s: %w", msg.Method, err)
	}
	return reply, nil
}

// readReply extracts the reply to request wantID from a response that is
// either one JSON message or a Server-Sent Events stream of them.
func readReply(resp *http.Response, wantID int64) (message, error) {
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))

	if mediaType != "text/event-stream" {
		var reply message
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxMessage)).Decode(&reply); err != nil {
			return message{}, fmt.Errorf("decoding reply: %w", err)
		}
		return reply, nil
	}

	// An SSE stream is a series of events separated by blank lines. Each
	// event has field lines; "data:" lines carry the payload, and several of
	// them in one event join with newlines. Other fields (id:, event:) and
	// comments (lines starting with ':') are not needed here.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxMessage)
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			continue
		}
		// A blank line ends an event.
		if len(data) == 0 {
			continue
		}
		var msg message
		err := json.Unmarshal([]byte(strings.Join(data, "\n")), &msg)
		data = data[:0]
		if err != nil {
			return message{}, fmt.Errorf("decoding event: %w", err)
		}
		// The stream may carry other messages (notifications, progress)
		// before the reply. Only the one answering our request ends it.
		if msg.ID != nil && *msg.ID == wantID {
			return msg, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return message{}, fmt.Errorf("reading event stream: %w", err)
	}
	return message{}, fmt.Errorf("event stream ended without a reply to request %d", wantID)
}
