package netx_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/fleveque/quantic-agent/internal/netx"
)

// wrap builds the chain net/http really returns for a failed dial: a
// *url.Error around a *net.OpError around a *os.SyscallError around the errno.
func wrap(errno error) error {
	return &url.Error{Op: "Get", URL: "http://127.0.0.1:1", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", errno),
	}}
}

func TestUnreachable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", wrap(syscall.ECONNREFUSED), true},
		{"connection reset", wrap(syscall.ECONNRESET), true},
		{"host unreachable", wrap(syscall.EHOSTUNREACH), true},
		{"network unreachable", wrap(syscall.ENETUNREACH), true},
		{"dropped before a reply", &url.Error{Op: "Post", URL: "http://x", Err: io.EOF}, true},
		{"wrapped again by a caller", fmt.Errorf("llm: GET /api/version: %w", wrap(syscall.ECONNREFUSED)), true},

		{"unknown host", &url.Error{Op: "Get", URL: "http://x", Err: &net.DNSError{Err: "no such host", Name: "x", IsNotFound: true}}, false},
		// A DNS error can carry an underlying cause (since Go 1.23). The rule is
		// about the name not resolving, whatever the DNSError wraps.
		{"DNS failure wrapping a network error", &net.DNSError{Err: "lookup failed", Name: "x", UnwrapErr: syscall.ENETUNREACH}, false},
		{"timeout", &url.Error{Op: "Get", URL: "http://x", Err: context.DeadlineExceeded}, false},
		{"cancelled", context.Canceled, false},
		{"some other error", errors.New("boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := netx.Unreachable(tt.err); got != tt.want {
				t.Errorf("Unreachable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
