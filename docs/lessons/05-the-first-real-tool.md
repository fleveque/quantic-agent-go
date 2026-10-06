# Lesson 05 — The first real tool

**Milestone 5** — the local model asked Quantic for the dividend calendar, got it, and answered from it.
Getting there took a protocol I'd only used through other people's clients, Go's reflection, the first
interfaces I actually needed, and an evaluation that taught me more about my agent than about the
models it was meant to compare.

*Also readable as a [formatted page](https://claude.ai/artifact/9Fe5FJdzbCf2jTGMyTdLNd). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/UJFF5Uiz7EwZJ75StoJwHE).*

---

## The question I thought was blocking

The design listed MCP authentication as the first blocker: Quantic's server uses OAuth, which assumes a
person clicking "Allow", and the agent is a daemon. I chose a service token for the public tools, so the
agent could never reach anyone's portfolio.

Then I read the server. My local checkout of Quantic was three months behind `origin/main`, which is why
my first search found no MCP code at all. After a fetch, the auth plug said it plainly:

```elixir
# No credentials at all: the request proceeds ANONYMOUSLY (rate
# limited). Public reference tools answer; scoped tools reply with a
# pointer to sign up — that error is itself the discoverability, ...
```

The thing I wanted from a service token, no user identity and so no portfolio access, is exactly what an
anonymous caller already gets, and the *server* enforces it. A token I don't have can't leak. So the
agent connects anonymously ([ADR 0006](../decisions/0006-anonymous-mcp-for-public-tools.md)), and the
service token waits until a tool needs an authenticated caller. Also: fetch before you grep.

## MCP, by hand first

I spoke the protocol with `curl` before writing any Go, so the client would be shaped by real replies.
MCP over HTTP is JSON-RPC 2.0: every request is a POST of one message. The first surprise was the
reply's shape:

```
$ curl -s -D - https://quantic.finance/mcp -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"initialize",...}'
HTTP/2 200
content-type: text/event-stream; charset=utf-8
mcp-session-id: session_GNvzcOdudTduZJ2e7AE=

id: 0
event: message
data: {"id":1,"jsonrpc":"2.0","result":{"capabilities":{"tools":{}},"protocolVersion":"2025-06-18",...}}
```

A Server-Sent Events stream, for a single reply. Ask with `Accept: application/json` only and the same
server answers with plain JSON. The spec says a client must accept both, so the Go client parses both,
and the tests exercise both. The second surprise was the tool's result:

```json
{"result":{"content":[{"type":"text","text":"{\"from\":\"2026-10-06\",\"days\":10,\"stocks\":[...]}"}],"isError":false}}
```

JSON inside a string inside JSON. The calendar has to be decoded twice.

Then I made things fail on purpose, and got three different shapes for three different layers:

| Failure | How it arrives |
|---|---|
| A portfolio tool called anonymously | HTTP 200, `isError: true`, an explanation as text |
| An unknown tool, or `"days": "ten"` | HTTP 200, a JSON-RPC `error` object, code `-32602` |
| A bad token | HTTP 401 |

The first two aren't failures of the *agent*. They're answers to show the model, so it can correct
itself. Only the third, and a server that isn't there, should stop a run. That distinction shaped the
whole loop.

## The struct is the schema

A model is shown a JSON Schema for each tool's arguments. I didn't want to write that schema by hand
next to the Go struct the arguments decode into, because two descriptions of the same thing will drift.
So the struct is the only one:

```go
type DividendCalendarArgs struct {
	Days int `json:"days,omitempty" max:"120" desc:"How many days ahead to look, counting from today. Defaults to 45; at most 120."`
}
```

`tools.SchemaOf` reads it with the `reflect` package: field names from the `json` tag, `omitempty`
meaning optional, `desc` becoming the description, `max` becoming `"maximum": 120`. This was my first
real use of reflection, and the thing that surprised me is how ordinary it is: `reflect.TypeOf(v)`, loop
over `t.NumField()`, read `field.Tag.Get("json")`. The same tags `encoding/json` has been reading since
lesson 02.

The reverse direction matters just as much. Whatever a model produces is decoded back into that struct,
strictly:

```go
dec := json.NewDecoder(bytes.NewReader(raw))
dec.DisallowUnknownFields()
```

`{"days":10,"sector":"Utilities"}` is an error, not a call with the sector silently dropped. The model
is told, and can try again.

A test also reads the schema the *server* publishes (captured in `tools-list.json`) and checks it against
ours. If Quantic renames an argument, the test fails in CI instead of the agent failing in production.

## The description is mine

The server describes `dividend_calendar` as "...Public — buy before the ex-date to receive the next
dividend." That's buying advice, and the agent's content must be informational only (design N5). What a
model is told a tool is *for* shapes what it writes, so the agent sends its own description. It's a small
thing that would have been easy to miss by passing the server's text straight through.

## The interfaces I promised in lesson 02

Lesson 02 ended with "an interface belongs to the consumer". Milestone 5 is the first time a consumer
needed one. The research loop declares exactly what it uses:

```go
type Model interface {
	Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
}

type ToolServer interface {
	CallTool(ctx context.Context, name string, arguments any) (mcp.Result, error)
}
```

`*llm.Client` and `*mcp.Client` satisfy them without knowing they exist, and the tests hand the loop a
scripted fake model and a fake server instead. One line in the loop makes sure the real clients keep
fitting:

```go
var _ Model = (*llm.Client)(nil)
```

It compiles to nothing. If `Chat`'s signature ever changes, the build fails there, rather than a program
failing later.

## The first real run

```
$ agent -research "Is Microsoft going ex-dividend this month? How much is the dividend?"
tool dividend_calendar {"days":31} → 1355 bytes (47ms)
Yes, Microsoft (MSFT) is going ex-dividend in this month.
*   **Ex-Dividend Date:** October 8, 2026
*   **Payment Frequency:** Quarterly

The specific dividend amount per share is not included in this calendar view; ...
```

The calendar has no amounts, and the model said so instead of reaching for one from memory. That's the
behaviour the whole design is built to get. The same week, though, it also told me Procter & Gamble's
16 October date was "outside the next 10-day window", when 16 October *is* the tenth day. The same
model was careful about a figure it didn't have and careless about a date it did. That's the design's
bet in miniature: give the model data, and keep the figures out of its prose.

## The evaluation that measured my agent

Decision 0005 said milestone 5 must leave behind an evaluation that any model can be put through.
`cmd/evaltools` asks eight questions whose right first move is known (call the calendar with about 7 days
for "this week", call nothing for "what is 17 times 23?") five times each per model.

| | qwen3.5:9b | qwen3.6:35b | Qwen3.8-27B IQ3_S |
|---|---|---|---|
| Well-formed calls | 40/40 | 40/40 | 40/40 |
| Right first move | 37/40 | 34/40 | 38/40 |

Every miss was the same: asked about "the next six months", the models requested 180 days from a tool
that covers at most 120. The description said so. They didn't care.

So I put the limit in the schema, as `"maximum": 120`, where a model should notice it, and ran it again.
Requests over 120: 3 → 3, 6 → 8, 2 → 3. No change. A limit written in the description or in the schema
is a suggestion.

What changed things was enforcing it in code. `DecodeArgs` now refuses `{"days":180}`, the loop shows
the model the refusal, and in three real runs out of three it retried with 120. The answers were another
matter:

```
run 1: "...the tools don't support looking further than 120 days ahead. Here are the stocks going
        ex-dividend in the next 4 months..."
run 2: "...here are the companies that have ex-dividend dates within the next 180 days..."
```

Run 2 had 120 days of data and claimed 180. There's the "180" in the answer, and no tool result it came
from. That's exactly what milestone 6's provenance validator rejects: a number in the output that no
recorded tool call accounts for. Milestone 6 starts with a real example.

As for comparing models: five runs per case can't tell these three apart. Nothing here overturns the 9B
as the default ([ADR 0005](../decisions/0005-default-model-by-measurement.md)). The evaluation earned its
keep anyway, by showing me where my *agent* needed to be stricter.

## What I'm taking into milestone 6

- Read the server before deciding how to talk to it. And fetch before reading: my checkout was three
  months stale.
- Speak a protocol by hand first. SSE for one reply, and JSON inside a string, aren't things I'd have
  guessed.
- Separate the failures the model can fix (show them to it) from the ones it can't (stop the run).
- One description of a tool's arguments: the struct. Reflection derives the schema; strict decoding
  holds the model to it.
- A rule a model must follow belongs in code. Description and schema are hints; the evaluation showed
  models ignore both at about the same rate.
- An interface belongs to its consumer, and `var _ I = (*T)(nil)` keeps the real types honest.

---

**Previous:** [Lesson 04 — Deadlines, and letting go cleanly](04-deadlines-and-letting-go.md) ·
**Next:** [Lesson 06 — Every figure has a source](06-every-figure-has-a-source.md)
