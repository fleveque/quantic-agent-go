package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fleveque/quantic-agent/internal/mcp"
)

// rateLimited answers the first refuse requests the way Quantic's MCP plug
// refuses an anonymous caller over its limit (QuanticWeb.Plugs.McpAuth,
// rate_limited/1: status 429, this JSON body, no Retry-After header), and
// every request after that with the captured calendar. It returns the count
// of requests it saw.
func rateLimited(t *testing.T, refuse int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID any `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&msg)
		if seen.Add(1) <= int64(refuse) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limited — try again shortly"}`))
			return
		}
		reply(t, w, "call-dividend-calendar.sse", msg.ID)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// fast is a backoff for tests: the same retries, without the waiting.
var fast = mcp.Backoff{Attempts: 3, Base: time.Millisecond, Max: time.Millisecond}

func TestARateLimitedRequestIsRetried(t *testing.T) {
	srv, seen := rateLimited(t, 2)
	c := mcp.New(srv.URL, "")
	c.Backoff = fast
	var retries []int
	c.OnRetry = func(method string, retry int, wait time.Duration) { retries = append(retries, retry) }

	res, err := c.CallTool(t.Context(), "dividend_calendar", map[string]any{"days": 10})

	if err != nil || res.IsError || res.Text == "" {
		t.Fatalf("CallTool = %+v, %v; want the calendar on the third try", res, err)
	}
	if seen.Load() != 3 || len(retries) != 2 || retries[1] != 2 {
		t.Errorf("server saw %d requests, retries reported %v; want 3 and [1 2]", seen.Load(), retries)
	}
}

func TestTheClientGivesUpOnARateLimit(t *testing.T) {
	srv, seen := rateLimited(t, 1000)
	c := mcp.New(srv.URL, "")
	c.Backoff = fast

	_, err := c.CallTool(t.Context(), "dividend_calendar", map[string]any{"days": 10})

	if !errors.Is(err, mcp.ErrRateLimited) {
		t.Errorf("err = %v, want ErrRateLimited", err)
	}
	if seen.Load() != 3 {
		t.Errorf("server saw %d requests, want exactly Attempts (3)", seen.Load())
	}
}

// A wait of an hour must still end the moment the run is cancelled.
func TestWaitingToRetryEndsWithTheContext(t *testing.T) {
	srv, _ := rateLimited(t, 1000)
	c := mcp.New(srv.URL, "")
	c.Backoff = mcp.Backoff{Attempts: 3, Base: time.Hour, Max: time.Hour}
	ctx, cancel := context.WithCancel(t.Context())
	c.OnRetry = func(string, int, time.Duration) { cancel() } // Ctrl-C, as the wait begins

	start := time.Now()
	_, err := c.CallTool(ctx, "dividend_calendar", map[string]any{"days": 10})

	if !errors.Is(err, context.Canceled) || errors.Is(err, mcp.ErrRateLimited) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("returned after %s, want at once", took)
	}
}
