# Lesson 03 — Errors you can ask questions of

**Milestone 3** — the agent can now tell "Ollama isn't running" from "that model isn't pulled" from
"the server fell over", without reading a single error message. Getting there meant learning the
three shapes an error takes in Go, and one bug in my own tests that only showed up when I ran them a
thousand times.

*Also readable as a [formatted page](https://claude.ai/artifact/CCH115UXhRfsN5N16eXRHf). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/6HdVkT2Z8FGZoyxMnyaJ5i).*

---

## The before picture

At the end of milestone 2, every failure in `internal/llm` was a `fmt.Errorf` string:

```
llm: GET /api/version: Get "http://localhost:11434/api/version": dial tcp [::1]:11434: connect: connection refused
llm: POST /api/generate: 404 Not Found: model 'no-such-model:latest' not found
```

Both read fine. Neither can be *acted on*. The design needs the agent to wait when Ollama is stopped
and fail when the model is missing ([design §3.6](../design.md#36-concurrency-model)), and the only
way to tell those apart was `strings.Contains(err.Error(), "connection refused")`. That breaks the day
Go rewords a message, and it would have been the first thing I'd have been embarrassed by in review.

In Elixir I'd have had `{:error, :econnrefused}` versus `{:error, {:http, 404, body}}` and pattern
matched. In Ruby, `rescue Errno::ECONNREFUSED` versus `rescue ModelNotFound`. Go has neither pattern
matching nor exceptions, so I needed Go's own answer.

## Three shapes of error

An `error` in Go is any value with an `Error() string` method. From that one interface, three
patterns grow:

| Shape | Looks like | Ask it with | Closest thing I knew |
|---|---|---|---|
| **Sentinel** | `var ErrUnavailable = errors.New("...")` | `errors.Is(err, llm.ErrUnavailable)` | An Elixir atom: `:unavailable` |
| **Error type** | `type APIError struct{ StatusCode int; ... }` | `errors.As(err, &apiErr)` | A Ruby exception class with attributes |
| **Wrapping** | `fmt.Errorf("llm: %s: %w", path, err)` | Both of the above see through it | Ruby's `cause`, but walked for you |

The rule that took me longest to accept: **a sentinel is compared with `errors.Is`, never `==`**.
Errors travel wrapped in context, so the value a caller receives is almost never the sentinel itself.
`errors.Is` unwraps layer by layer until it finds a match or runs out.

That even applied to code I'd already written. `if err == flag.ErrHelp` in `cmd/agent` works today
because the flag package returns the sentinel unwrapped, but nothing promises that. It's
`errors.Is(err, flag.ErrHelp)` now.

## What actually fails

Before deciding what "unavailable" means, I made each failure happen and looked at what Go's HTTP
client handed back. I didn't want to guess:

```
closed port          dial tcp 127.0.0.1:32825: connect: connection refused   ECONNREFUSED
dropped mid-request  Post "http://127.0.0.1:36099": EOF                      io.EOF
unknown host         lookup no-such-host.invalid: no such host               *net.DNSError
no network           connect: network is unreachable                         ENETUNREACH
too slow             context deadline exceeded (Client.Timeout exceeded ...) (neither)
```

That table *is* the design decision:

- **Refused, dropped, unreachable network → `ErrUnavailable`.** A stopped Ollama refuses; a
  restarting one drops the connection. Both mean "try again later".
- **Unknown host → not unavailable.** An unresolvable name is almost always a typo in `OLLAMA_HOST`.
  If the agent treated that as "wait", it would wait forever for a machine that doesn't exist. A typo
  should fail loudly.
- **Timeout → not unavailable.** A slow server isn't a missing one. Milestone 4 owns deadlines.

## Two sentinels, one type

```go
var (
	ErrUnavailable   = errors.New("model server unavailable")
	ErrModelNotFound = errors.New("model not found")
)

type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
	Err        error // ErrModelNotFound when recognised, else nil
}

func (e *APIError) Unwrap() error { return e.Err }
```

The sentinel messages have no `llm:` prefix on purpose. They're never printed alone; they're always
inside a message that already starts with `llm:`, and a prefix would print twice.

`APIError` copies the shape of the standard library's `*os.PathError` (`Op`, `Path`, `Err`): details
as fields, and the *kind* of failure as a wrapped error. The `Unwrap` method is what `errors.Is` and
`errors.As` call to look inside. So one value answers both questions: "is this a missing model?"
(`errors.Is`) and "what status did the server send?" (`errors.As`).

**Telling a missing model from a missing endpoint.** Both are 404s. What differs is who answered:

```
$ curl -s localhost:11434/api/generate -d '{"model":"no-such-model:latest","prompt":"hi","stream":false}'
{"error":"model 'no-such-model:latest' not found"}          HTTP 404
$ curl -s localhost:11434/api/nope
404 page not found                                          HTTP 404
```

Ollama's handlers answer in JSON; the router in front of them answers a wrong path in plain text. So
a 404 counts as `ErrModelNotFound` only when the body is Ollama's JSON. That first reply is now a test
fixture, captured from this machine.

## Two `%w` in one message

Go 1.20 allowed more than one `%w` in a single `fmt.Errorf`:

```go
if unreachable(err) {
	return fmt.Errorf("llm: %s %s: %w: %w", req.Method, req.URL.Path, ErrUnavailable, err)
}
```

The result answers `errors.Is(err, llm.ErrUnavailable)` *and* `errors.Is(err,
syscall.ECONNREFUSED)`. The agent asks the first; the underlying cause stays available for anyone who
needs it, and for the log. One wrapped error per layer used to be the rule, and I'd have had to pick.

## `errors.As` wants a pointer to a pointer

```go
var apiErr *llm.APIError
if errors.As(err, &apiErr) && apiErr.StatusCode >= 500 {
```

This looked wrong to me the first time: `apiErr` is already a pointer, and I pass its address. It's
the same reason as `&out` in [lesson 02](02-structs-tags-and-one-http-call.md):
`errors.As` has to *set my variable*, so it needs my variable's address. The variable's type,
`*llm.APIError`, is what it searches the chain for. It has to be the pointer type, because the
`Error` method is defined on `*APIError`: that's the type that is an `error`, not `APIError`.

## The trap I stepped around

`apiError` builds and returns an `*APIError`, and `do` returns it as an `error`. That's fine because
it's never nil. If it could ever return a nil `*APIError`, this would happen:

```
$ go run .      # a function returning (*APIError)(nil) as an error
err == nil: false
err holds: *main.APIError true
```

An interface value is nil only when it holds *nothing*. A nil pointer of a concrete type is still
something: the interface knows its type. So `err != nil` would be true, and the caller would report
a failure that never happened. The rule I took from it: a function that returns `error` should
`return nil` literally, never a typed variable that happens to be nil.

## The caller decides

The library classifies; the command decides what each kind means to a person:

```go
switch {
case errors.Is(err, llm.ErrUnavailable):
	// "Is Ollama running?", exit 3
case errors.Is(err, llm.ErrModelNotFound):
	// "Pull it with: ollama pull X", exit 1
}
// anything else: print it; a 5xx also points at the server's log
```

Against the real server on this machine:

```
$ agent -ollama localhost:1 -check
agent: no model server answering at localhost:1. Is Ollama running?
agent: llm: GET /api/version: model server unavailable: Get "http://localhost:1/api/version": dial tcp [::1]:1: connect: connection refused
exit 3

$ agent -model no-such-model:latest -ask hi
agent: no-such-model:latest is not on this server. Pull it with: ollama pull no-such-model:latest
exit 1

$ agent -ollama no-such-host.invalid:11434 -check
agent: llm: GET /api/version: Get "http://no-such-host.invalid:11434/api/version": dial tcp: lookup no-such-host.invalid: no such host
exit 1
```

**Exit status 3** is new, and it's for the future scheduler: it means *nothing was attempted*, so
running the same command later is safe. That's the first half of "a stopped Ollama means wait". The
waiting itself arrives with the daemon.

`cmd/bench` got the same treatment with a Go feature I hadn't used: a **labelled break**. The loop
over models is labelled `models:`, and when a measurement comes back `ErrUnavailable`, `break models`
leaves both loops at once. The benchmark then reports what it measured before the server went away,
rather than printing the same failure for every remaining model and context size.

## A thousand runs found a bug from milestone 2

The new tests passed. Then one run of the whole suite failed, and the next run passed. I didn't want
to call it a fluke, so I ran it a thousand times:

```
$ go test -race -count=1000 -cpu 1,8 ./internal/llm/ ./cmd/agent/
--- FAIL: TestModels (0.00s)
    ollama_test.go:344: path = "/api/generate", want /api/tags
--- FAIL: TestRunReportsAnUnreachableServer (0.00s)
    main_test.go:163: [-ollama http://127.0.0.1:45581 -ask hi]: stderr = "quantic-9b:latest · 2 tokens · 205ms\n", want it to suggest checking the server
```

`TestModels` receiving a *generate* request makes no sense, until you notice that `go test ./...`
runs each package's test binary **at the same time**. To get a "dead" address, my milestone 2 test
started a server and closed it, trusting that nothing would listen on that port afterwards. But the
port goes back to the operating system, which can hand it to the *other* test binary's next server a
moment later. The "dead" address was alive, belonged to another test, and answered.

The fix is an address that can't be handed out: `127.0.0.1:1`. Port 1 is privileged, outside the
range the OS assigns to test servers, and nothing listens on it on a normal machine. After that, 2,000
runs of each package passed.

The same stress run caught a second, smaller thing. The benchmark test asserted *exactly* three
requests reached the server, and sometimes it saw four. I checked which model the fourth was for
before touching anything: always the same model, never the next one. Go's HTTP transport sometimes
resends a request by itself when a connection drops at the wrong moment. The bench logic was right;
the test was asserting something the transport doesn't promise. It now asserts what matters: the
second model is never requested.

**The rule I took from it:** a test that fails once in a hundred runs is telling you something. Run
it a thousand times before deciding what.

## What I'm taking into milestone 4

- Sentinels for *kinds* of failure, a struct type for *details*, `%w` to carry both. Compare with
  `errors.Is` and `errors.As`, never `==` or string matching.
- Decide what an error means from the outside: reproduce the failure and look at what comes back.
- Two `%w` in one `Errorf` keep both our classification and the underlying cause.
- Return `nil` literally from a function returning `error`, never a nil pointer of a concrete type.
- `-count=1000` is cheap. A flaky test is a bug report.
- Milestone 4 adds `context` to every call. A deadline being exceeded must stay a *different* error
  from `ErrUnavailable`: slow is not gone.

---

**Previous:** [Lesson 02 — Structs, tags, and one HTTP call](02-structs-tags-and-one-http-call.md) ·
**Next:** [Lesson 04 — Deadlines, and letting go cleanly](04-deadlines-and-letting-go.md)
