# Lesson 04 — Deadlines, and letting go cleanly

**Milestone 4** — every call to the model now carries a deadline that the caller chooses, and the agent
stops cleanly on Ctrl-C or when systemd stops it. I measured what Ollama does when a client gives up
before writing any of it, and one measurement changed how the default had to be chosen.

*Also readable as a [formatted page](https://claude.ai/artifact/LCHhkZQtTehnjSNXRyVWJv). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/LvtwYVQmph38YeYdrjSRte).*

---

## The before picture

Milestone 2 left one line standing in for every timeout in the program:

```go
http: &http.Client{Timeout: 5 * time.Minute},
```

Every call got the same five minutes: asking the server its version, which takes milliseconds, and
generating at 64K context on a model half in system RAM, which took 94 seconds in the benchmark. The
caller had no say. And there was no way to *stop* a call from outside: the only exit was the process
dying.

## What happens when a client gives up

Before choosing anything, I gave up on Ollama on purpose and read its log.

**Mid-generation**, with the model already loaded:

```
$ curl --max-time 4 localhost:11434/api/generate -d '{... "num_predict":4000}'
curl exit 28 after 4008 ms
91 %                       ← GPU, one second later
0 %
srv          stop: cancel task, id_task = 3
slot      release: id  0 | task 3 | stop processing: n_tokens = 396
```

Ollama notices the closed connection and stops generating within about a second. Cancelling a request
gives the GPU back.

**During a cold load**, the answer was different:

```
msg="client connection closed before llama-server finished loading, aborting load"
msg="Load failed" ... error="timed out waiting for llama-server to start: context canceled"
```

Cancelling while the model is still loading **aborts the load**. That changes how a timeout has to be
chosen: a deadline shorter than a cold start doesn't just fail once. It aborts the load, and every
retry starts loading from zero again, and fails again.

So the default came from measurements, not from a round number:

| | Measured |
|---|---|
| Cold load, qwen3.6:35b at 32K | 30.7 s |
| Cold load, Qwen3.8-27B IQ3_S at 32K | 27.7 s |
| Slowest single request in the benchmark | 93.5 s |

Five minutes stayed the agent's default: more than double the worst case, load included. The
difference is that it's now a flag, and the caller's decision.

## `context.Context`, the shape of it

A `context.Context` is a value you pass down through every call that might wait. It carries a
deadline (or not) and a "done" signal that whoever created it can trigger. The rules took me a while to
take on board:

- **It's the first parameter**, named `ctx`, on every function that does I/O. `Generate(ctx, req)`,
  not `Generate(req, ctx)`. This is so universal in Go that I had to rename an `int` called `ctx` in
  `cmd/bench` (it was the context *size*), because to any Go reader `ctx` means a context.
- **You derive, you don't mutate.** `context.WithTimeout(parent, 5*time.Minute)` returns a *new*
  context that ends at the deadline *or* when the parent ends, whichever is first. A request-level
  deadline inside a run-level cancel inside a signal handler: each layer narrows the one above it.
- **Every derived context comes with a `cancel` function you must call**, usually with `defer`. It
  releases the timer. Leave `cancel` unused and the compiler refuses; throw it away with `_` and
  `go vet` says "the cancel function returned by context.WithTimeout should be called, not discarded,
  to avoid a context leak".

Coming from Elixir, this felt like doing by hand what a supervisor and `Task.await(task, timeout)`
give you for free. But the explicitness pays: reading `Generate(ctx, ...)` tells you the call can be
stopped, and by whom, without looking anywhere else.

## The library takes a context; the caller decides

The change to `internal/llm` is small. Each method takes `ctx` and builds its request with it:

```go
req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
```

and the client's own `Timeout` is gone. A library that imposes its own deadline takes a decision away
from the one place that knows the right answer: the caller, who knows whether this is a version check or
a 64K research step.

The part that needed thought was the error. When a context ends, the HTTP client tears the connection
down, and milestone 3 classifies some torn-down connections as `ErrUnavailable`. Getting that wrong would
be wrong in the worst way: "the server is gone, wait and retry" for a server that's merely slow. So `do`
asks the context first:

```go
if ctxErr := req.Context().Err(); ctxErr != nil {
	return fmt.Errorf("llm: %s %s: %w", req.Method, req.URL.Path, ctxErr)
}
```

Then I checked whether it was needed. With the check deleted, the timeout and cancellation tests still
passed 200 runs out of 200: Go's HTTP client already wraps the context's error in what it returns. I
kept the check anyway, and the lesson is why. Without it, "a timeout is not unavailable" is true because
of how another package happens to build its errors today. With it, the priority is written down where
the classification happens: if the caller gave up, that's the reason, whatever the connection said on
its way out. A test pins the behaviour either way: a timed-out call must answer
`errors.Is(err, context.DeadlineExceeded)` and must *not* answer `errors.Is(err, llm.ErrUnavailable)`.
Slow is not gone.

## Stopping cleanly

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
context.AfterFunc(ctx, stop)
os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
```

`signal.NotifyContext` turns Ctrl-C (SIGINT) and SIGTERM, which is what systemd sends to stop a service,
into a context that's cancelled when the signal arrives. Everything downstream already listens to that
context, so the request in flight is cancelled and `run` returns normally.

`context.AfterFunc(ctx, stop)` is the line I'd never have thought of. After the first signal, it hands
signal handling back to Go's default. So a *second* Ctrl-C kills the process immediately, for when the
clean stop is taking longer than your patience.

Against the real server:

```
$ agent -ask "Write a 3000-word essay about dividend investing."      (SIGINT after 3s)
agent: stopped; the request in flight was cancelled
exit 130
srv          stop: cancel task, id_task = 3

$ agent -model qwen3.6:35b -timeout 3s -ask hi                         (cold model)
agent: gave up after 3s (-timeout). A cold model can take half a minute to load, and a long generation longer.
exit 1
msg="client connection closed before llama-server finished loading, aborting load"
```

130 is 128 + 2 (SIGINT's number), the shell's own convention for "stopped by Ctrl-C".

## What I almost claimed

I was about to write that this milestone is what frees the GPU when you stop the agent. So I checked
what happened *before* it: I killed a client with `kill -9`, which allows no clean-up at all.

```
curl killed with SIGKILL
0 %
srv          stop: cancel task, id_task = 579
```

Ollama stopped anyway. When a process dies, the operating system closes its connections, and Ollama
sees a closed connection either way. So killing the agent always freed the GPU.

What this milestone actually adds is the *clean* part. The agent says what happened, exits with a code a
scheduler can read, and runs its deferred clean-up. Today that clean-up is trivial; from milestone 7 it
saves the run's progress to SQLite, and "stopping loses no work" (design §3.6) depends on it. A killed
process can't do that. A cancelled one can.

## The test that hung

My first fake server for "a model that takes forever" looked like this:

```go
srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done() // wait until the client gives up
}))
```

The client gave up at 50ms. The test hung until the 10-second timeout killed it. Go's HTTP server
cancels `r.Context()` when the client hangs up, but it only *notices* the hang-up once it has read
the request body. My handler never read the body, so for the server the client was still sending, and
the handler waited forever. One line fixed it:

```go
io.Copy(io.Discard, r.Body) // read the request, as a real server does
<-r.Context().Done()
```

Ollama reads the request before it works on it, so the fake now behaves like the real thing. The rule I
took from it: a fake server should do what the real one does before it does what the test needs.

## What I'm taking into milestone 5

- `ctx` first, on everything that waits. Derive with `WithTimeout`/`WithCancel`, always `defer cancel()`.
- The caller owns the deadline. A library that sets its own timeout is deciding for someone it can't see.
- Check `ctx.Err()` before classifying a failure: the reason the caller stopped beats whatever the
  connection said.
- Measure what the other side does when you give up. Here it decided the default: a cancelled
  generation frees the GPU, but a cancelled load starts over.
- Check the "before" before claiming the "after". `kill -9` already freed the GPU; the clean stop is
  what's new.
- Milestone 5 is the first real tool call, and the research loop that grows from it will make dozens of
  model calls per run, each with its own deadline under the run's.

---

**Previous:** [Lesson 03 — Errors you can ask questions of](03-errors-you-can-ask-questions-of.md) ·
**Next:** Lesson 05 — the first real tool *(not written yet)*
