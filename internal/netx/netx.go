// Package netx holds the one network judgement both clients make: whether a
// failed request means the server simply isn't there.
//
// internal/llm (Ollama) and internal/mcp (Quantic) each turn that answer into
// their own ErrUnavailable. The rule lives here once, because it is subtle
// (which errors count, and why a DNS failure doesn't), and two copies of it
// would drift.
package netx

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// Unreachable reports whether a transport error means the server wasn't there
// to answer. Three cases were reproduced: connection refused (nothing
// listening), EOF (the connection closed before a reply, as a restart does)
// and network unreachable (no route to the server's address). A reset
// connection and an unreachable host are the same situation seen from
// elsewhere. A DNS failure is left out on purpose: an unknown host is far more
// often a typo in OLLAMA_HOST than a machine that's off, and a typo should fail
// loudly rather than be waited on. A cancelled or expired context should be
// checked before calling this: a slow server is not a missing one.
func Unreachable(err error) bool {
	// Checked first, so the rule is about the name failing to resolve and
	// doesn't depend on what a *net.DNSError happens to wrap.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	for _, cause := range []error{
		syscall.ECONNREFUSED, syscall.ECONNRESET,
		syscall.EHOSTUNREACH, syscall.ENETUNREACH,
		io.EOF,
	} {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
}
