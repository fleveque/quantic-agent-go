# quantic-agent

A local, autonomous content and QA agent for [Quantic](https://quantic.finance), written in Go.

It runs on my own hardware, drives a **local LLM** (no API bills, no data leaving the machine),
pulls **real financial data** from Quantic's MCP server, and turns it into draft content, data-quality
reports and small code changes — delivered as **pull requests and review-queue entries, never as
anything published automatically**.

> **Status: milestone 7.** The agent calls real tools, checks its own answers and remembers what it
> did: `agent -research` lets the local model ask Quantic's MCP server for data, verifies that every
> figure in the answer traces to a tool result, and records the run and every tool call in SQLite.
> `agent -run N` re-checks any past answer against exactly the data it was given. `agent -check` reports the model server and its models, every call has a deadline, and
> Ctrl-C or a service stop cancels the request in flight. `cmd/evaltools` measures how well each model
> picks and calls tools. No scheduled tasks yet. Slow on purpose —
> the project doubles as my way of learning Go in public, so the commit history *is* the learning record.
> See the [roadmap](#roadmap) for where it's going and [docs/lessons](docs/lessons) for what each step taught me.

---

## The one rule

> **The LLM never invents a number.**

Every yield, valuation, dividend amount and price change in generated output must trace back to a
recorded tool call against Quantic's real data. The model is allowed to *write prose*, *summarise*,
*spot patterns* and *write code*. It is never allowed to recall a figure from its training data.

This isn't a guideline in a prompt — it's enforced. Every draft the agent produces carries a
**provenance manifest** of the tool calls that fed it, and a validator rejects any draft containing a
numeric token that doesn't appear in that manifest. A draft that fails provenance never reaches the
review queue.

The second rule follows from the first: **nothing ships without a human.** No auto-merge to `main`,
no auto-posting to Telegram or Reddit. Output lands as a PR or a `pending_review` row, and waits.

---

## Why this exists

Quantic is a dividend-portfolio tracker (Elixir/Phoenix, real users, real money decisions). It needs a
steady stream of data-grounded content — valuation write-ups, dividend digests, stock comparisons — and
it needs someone watching for bad data in ~thousands of instruments. Both jobs are repetitive, both
benefit from an LLM, and neither can tolerate hallucinated figures.

A local model on my own GPU makes the economics work: the agent can run all day, burn as many tokens as
it likes, and never send a user's portfolio to a third party.

## Why Go

Honest answer: **I want to learn Go**, and this is a problem shaped exactly like Go's strengths.

- **Concurrency that matters.** One GPU means LLM inference is a strictly serialised resource, while
  tool calls against Quantic are I/O-bound and want to fan out. That tension — a semaphore of one
  around the model, an `errgroup` around the data fetches, `context` cancellation threading through
  both — is a real concurrency design, not a toy example.
- **Static types for an untyped boundary.** LLM tool-calling is JSON soup. Go's type system plus
  explicit schema definitions turn that boundary into something that fails loudly at the edge instead
  of silently three layers in.
- **A single static binary.** Drop it on the machine, point systemd at it, done. No runtime, no venv.
- **Mature libraries** for exactly this: `go-github`, `modernc.org/sqlite` (pure Go, no cgo),
  `log/slog`, and `net/http` good enough that an LLM client needs no framework.

I write Elixir and Ruby daily. Go's explicit error handling, structural interfaces and share-memory
concurrency are genuinely different models, which is the point.

---

## Architecture

```
                    ┌───── RESEARCH (agentic, bounded) ─────┐
   ┌──────────┐     │                                       │
   │ local LLM│◄───►│   model ⇄ read-only Quantic tools  ×N  │──► manifest
   │ Qwen·GPU │     │   budgets: calls · time · tokens       │       │
   └──────────┘     └───────────────────────────────────────┘       │
        ▲                                                            ▼
        │                                              ┌──────────────────────┐
        └──────────────────────────────────────────────│  WRITE (no tools)    │
                                                       │  prose from manifest │
                                                       └──────────┬───────────┘
                                                                  ▼
                                                       ┌──────────────────────┐
                                                       │ PROVENANCE VALIDATOR │
                                                       │ every number traced  │
                                                       └──────────┬───────────┘
                                                    ┌─────────────┴────────────┐
                                                    ▼                          ▼
                                            ┌──────────────┐          ┌────────────────┐
                                            │ SQLite queue │          │  GitHub PR     │
                                            │ + 7 locales  │          │  (never merged)│
                                            └──────┬───────┘          └───────┬────────┘
                                                   └───────────┬──────────────┘
                                                               ▼
                                                       human review — always
```

The split is the whole design: **research is a real agentic loop** where the model picks tools and
follows threads, bounded by call/time/token budgets over a read-only allowlist. **Writing has no tools
at all** — it receives the accumulated manifest and nothing else, so it cannot wander into invention.
Delivery tools live outside the loop entirely. See
[decision 0001](docs/decisions/0001-agentic-research-constrained-writing.md).

### Components

| Package | Responsibility |
|---|---|
| `cmd/agent` | Entrypoint, flag/config parsing, daemon loop and graceful shutdown |
| `cmd/bench` | Measures prompt/generation throughput and GPU residency per model, on the machine it runs on |
| `cmd/evaltools` | Measures how reliably each model picks the right tool with valid arguments, on fixed questions |
| `internal/llm` | Local model client (Ollama HTTP API), tool-call schema, structured-output decoding |
| `internal/mcp` | Client for Quantic's MCP server — the only source of financial facts |
| `internal/agent` | The research loop: tool dispatch, budget accounting, retries, phase state machine |
| `internal/tools` | Tool registry and JSON schemas generated from Go structs; read-only allowlist vs delivery tools |
| `internal/netx` | The one network judgement both clients share: is the server simply not there? |
| `internal/rag` | Embeddings in SQLite BLOBs, brute-force cosine similarity, style memory over approved drafts |
| `internal/provenance` | Records every tool call; validates that generated numbers trace back to one |
| `internal/tasks` | Task definitions (Week Ahead, raise/cut notes, valuation write-up, data QA) and schedules |
| `internal/i18n` | Translation pass (prose only) and the translation validator across 7 locales |
| `internal/post` | The output format: typed frontmatter + prose, serialisation, fold marker |
| `internal/store` | SQLite: review queue, task history, full tool-call audit log |
| `internal/ghpr` | `go-github` helper: branch, commit, open PR — no push to `main`, ever |

### Stack decisions

- **Inference: Ollama** over raw `llama.cpp server`, for its tool-calling API and model management.
  (I already maintain [llm-kit](https://github.com/fleveque/llm-kit) for the llama.cpp path if I need
  more control later.)
- **Model: Qwen3.5-9B by default, chosen by measurement.** On the target card it stays entirely in
  VRAM up to 64K context and is the fastest candidate by a wide margin. Bigger models are candidates,
  not defaults: a mixture-of-experts `qwen3.6:35b` runs fast even half in system RAM, and Qwen3.8-27B
  at ≈3.5 bits fits only at short contexts. What decides is quality on the agent's own tasks, measured
  from milestone 5 — and re-measured as new models appear
  ([decision 0005](docs/decisions/0005-default-model-by-measurement.md)). Nothing is baked in:
  `-model` and `QUANTIC_MODEL` choose, and `agent -check` reports what the target machine actually has.
- **Structured output over native tool-calling.** Local models are noticeably flakier at tool-calling
  than frontier models. The plan is to lean on JSON-schema-constrained decoding and treat native
  tool-calling as an optimisation, not a foundation.
- **SQLite via `modernc.org/sqlite`** — pure Go, no cgo, keeps the static-binary property.

### Hardware

Ryzen-class desktop, 64GB RAM, NVIDIA RTX 4070 Ti Super (16GB VRAM). The agent is designed to run
opportunistically: work while the machine is on, checkpoint state, resume cleanly.

That machine is the deployment target, not where this is written, so the binary assumes nothing about
which models are present — `agent -check` asks the server it's pointed at.

---

## What it publishes

The flagship format, built and proved first:

**The Dividend Week Ahead** — weekly. *N companies go ex-dividend this week* (table: ticker, ex-date,
amount, yield, safety badge), *raises declared* with old → new, *cuts and at-risk flags* from the
existing dividend-safety work, *radar movers*, and one short "what to watch" paragraph. Published to
`/insights` across all seven locales from one human review, plus a social variant sharing the same
manifest.

The agent emits **typed data plus prose**, never HTML and never markdown tables — a Phoenix template
per post kind renders it through the components Quantic already has, so posts look like Quantic and a
redesign never means regenerating content. Figures live in structured fields, prose contains no
numbers at all, which makes provenance validation exact and removes locale number formatting from the
agent's problem entirely. See the [format contract](docs/rendering.md).

Then, in order: **raise & cut notes** (event-driven, a raise is news the day it's declared),
**valuation deep-dives** (evergreen, where retrieval over filings earns its place), and a **monthly
dividend health report**.

Deliberately weekly, not daily — Google's scaled-content-abuse policy targets bulk machine-generated
pages, and quantic.finance is a real domain with ranking pages already earning traffic. Full reasoning,
formats and the registration-gate design in the [content plan](docs/content.md).

Alongside the content: **data QA** (nulls, outliers, impossible dates across the instrument universe →
GitHub issues or small mechanical PRs) and **low-risk code work** (refactors, tests, fixtures, docs).
*Never:* business logic, financial calculations, or anything touching real user money.

## Guardrails

- No automatic merge to `main`. The agent's GitHub token is scoped to branch + PR creation.
- No automatic publishing to any external channel.
- Every numeric claim traces to a logged tool call, or the draft is rejected.
- Every tool call is logged with inputs and outputs, so any output can be audited after the fact.
- Token and wall-clock budgets per task; a runaway task is cancelled, not left running.
- No user portfolio data in logs, fixtures, prompts, or anything committed to this repo.

---

## Roadmap

Each milestone ships working code *and* a lesson write-up in [`docs/lessons`](docs/lessons), because
the point is learning Go, not just having an agent.

| # | Milestone | Go ground covered |
|---|---|---|
| 0 | Repo, design, decisions | — |
| 1 | Hello, module: layout, `cmd/` vs `internal/`, first test | modules, packages, visibility, `go test` |
| 2 | Ollama client: send a prompt, decode the response | structs, JSON tags, interfaces, `net/http` |
| 3 | Error handling across the LLM boundary | `error` values, wrapping, `errors.Is/As`, sentinels |
| 4 | Timeouts and cancellation for slow generations | `context`, deadlines, graceful shutdown |
| 5 | First real tool: `dividend_calendar` end to end | schema from structs, reflection, MCP auth |
| 6 | Provenance validator + table-driven tests | slices/maps, `testing`, `httptest`, fakes |
| 7 | SQLite: runs, drafts, audit log *(you are here)* | `database/sql`, migrations, transactions |
| 8 | **The agentic research loop** — dispatch, budgets, retries | state machines, backoff, `context` in a loop |
| 9 | Worker pool: serialised GPU, parallel I/O | goroutines, channels, `sync`, `errgroup`, semaphores |
| 10 | **Retrieval**: embeddings, brute-force cosine, style memory | `[]float32` math, `testing.B`, BLOBs |
| 11 | Week Ahead end to end, 7 locales, translation validator | time, `embed`, YAML marshalling, struct tags |
| 12 | GitHub PR flow | `go-github`, auth, third-party module ergonomics |
| 13 | Ship it: binary, `log/slog`, systemd unit | build flags, cross-compilation, structured logging |

## Working on this

`main` is protected: no direct pushes, every change goes through a pull request, and CI must pass
before merge. Same discipline as the main Quantic repo — worth having on a solo project precisely
because there's nobody else to catch a bad push.

CI runs `gofmt`, `go vet`, `go build` and `go test -race`, plus a check that relative links in the
docs still resolve. The same checks locally:

```sh
gofmt -l .              # lists unformatted files; empty output is a pass
go vet ./...
go test -race ./...
go run ./cmd/agent -version
go run ./cmd/agent -check              # is the model server up?
go run ./cmd/agent -ask "say hello"    # one prompt, one reply
go run ./cmd/agent -research "What goes ex-dividend this week?"   # model + Quantic tools
go run ./cmd/agent -runs                # recent runs, from the SQLite history
go run ./cmd/agent -run 3               # one run's tool calls and answer, re-checked
go run ./cmd/bench                     # tokens/second and GPU residency per model
go run ./cmd/evaltools                 # how reliably each model calls tools
```

`cmd/bench` is meant for the machine the agent will actually run on: it reports prompt and
generation rates per model and context size, and how much of each model stayed in VRAM. Numbers from
a development laptop say nothing useful about the deployment box. Setting up that machine, pulling
the models and running the benchmark is in [docs/target-machine.md](docs/target-machine.md).

## Lessons

Written as I go, in [`docs/lessons`](docs/lessons). They're notes from someone coming to Go from
Elixir and Ruby — what surprised me, what I got wrong first, and why Go does it that way.

## Documents

- [`docs/design.md`](docs/design.md) — architecture, provenance, retrieval, translation, open questions
- [`docs/content.md`](docs/content.md) — what gets published, cadence, locales, the registration gate
- [`docs/rendering.md`](docs/rendering.md) — the format contract: typed data + prose, rendered by Quantic components
- [`docs/target-machine.md`](docs/target-machine.md) — runbook for the desktop the agent runs on: setup, models, benchmark
- [`docs/decisions/`](docs/decisions) — architecture decision records (why the design is what it is)
- [`docs/lessons/`](docs/lessons) — the Go lessons

## Licence

[MIT](LICENSE). The agent is MIT; Quantic itself is private and proprietary — this repo only talks to it.
