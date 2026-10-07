# quantic-agent — design

Living document. Records decisions, the reasoning behind them, and the questions still open.
Originates from a Spanish-language idea sketch; this is the refined English version the
implementation follows.

Companion documents: [content plan](content.md) · [format & rendering](rendering.md) · [decisions](decisions/)

---

## 1. Problem

Quantic (quantic.finance) is a dividend-portfolio tracker: Elixir/Phoenix monolith, real users, real
money decisions. Recurring jobs that are repetitive, LLM-shaped, and intolerant of hallucination:

1. **Content.** Recurring, data-grounded publishing — a weekly dividend digest, raise/cut notes,
   valuation write-ups — across seven locales. See the [content plan](content.md).
2. **Data quality.** Watching a large instrument universe for nulls, outliers, impossible dates and
   suspicious dividend histories.

The agent is meant to keep running and take on more jobs of this kind, not only to write articles. One
is about the agent itself:

3. **Model upkeep.** Open models improve every few months, and a newer model is often better than an
   older, bigger one. Watch for new candidates, measure them on this machine and on this agent's own
   tasks, and propose a change of default — as a PR carrying the numbers, never by switching itself
   (N2). See [decision 0005](decisions/0005-default-model-by-measurement.md) for the evaluation routine,
   and [open question 10](#5-open-questions).

Frontier-model APIs would work but cost per token and send portfolio-adjacent data to a third party.
A local model on owned hardware removes both constraints, at the cost of lower model quality — which
is precisely why the architecture must not depend on the model being smart about facts.

## 2. Non-negotiables

**N1 — No invented numbers.** Every figure in output traces to a recorded tool call. Enforced by a
validator, not by prompt instructions.

**N2 — No autonomous publication.** No merge to `main`, no post to any channel. Output is a PR or a
`pending_review` row.

**N3 — Full auditability.** Every tool call logged with inputs and outputs. Any output can be
reconstructed after the fact: what did the agent see, and when.

**N4 — No user data leaves the machine, or lands in this repo.** No portfolio contents in prompts,
logs, fixtures, or commits.

**N5 — Informational, never advisory.** Content describes what happened and what the data says. It
does not recommend buying or selling. Quantic operates in the EU; "this stock is a buy" is a
regulatory problem, not a style preference.

## 3. Architecture

### 3.1 The core split: agentic research, constrained writing

The pipeline has two LLM phases with deliberately different freedom:

```
        ┌─────────── RESEARCH (agentic) ───────────┐   ┌── WRITE ──┐
trigger →  plan → [ model ⇄ read-only tools ] ×N   → manifest → prose → validate → deliver
        └── bounded: calls · wall-clock · tokens ──┘   └ no tools ─┘
```

**Research is a real agentic loop.** The model chooses which tools to call and can follow a thread —
*"this yield jumped 40%, so check the payout ratio, then the last five dividends, then whether the
price collapsed"* — accumulating a manifest as it goes. Every call is read-only, so the blast radius
is zero and the loop can be given genuine freedom.

**Writing has no tools at all.** A separate model call receives *only* the accumulated manifest and
writes prose. Because it cannot fetch, it cannot wander mid-sentence into invention. It also cannot
"remember" a figure it never saw.

**Delivery tools (`create_pr`, `enqueue_for_review`) live outside the loop.** They are reachable only
at the terminal step and are never in the research allowlist, so no amount of wandering can cause the
agent to publish something.

An earlier draft of this design made the whole pipeline fixed — the task declared its data
dependencies up front and the model only wrote prose. That was over-cautious: the provenance manifest
records tool calls regardless of *who* decided to make them, so the N1 guarantee survives an agentic
loop intact. What a fixed pipeline actually buys is predictable cost, and that's what budgets are
for. See [decision 0001](decisions/0001-agentic-research-constrained-writing.md).

**As built (milestone 8, [`internal/agent`](../internal/agent/)).** `Researcher.Research` is the loop;
when the model stops asking for tools, whatever it says is discarded. `Writer.Write` is one model
call with no tools, given the question and each successful call's result, labelled with the call
that produced it. Measured against milestone 7's single loop on the same questions and data, the
split traced as many answers (6 of 9 each) and failed differently
([benchmarks](benchmarks/2026-10-07-writer/README.md)): the writer doesn't know today's date, and it
miscounts in words ("ten" stocks where there are nine), which the validator doesn't read. A variant
that showed the writer the research conversation gave buy-timing advice twice; the data-only writer
didn't.

### 3.2 Loop bounds

The research loop is bounded, not open-ended. Per task kind, from config:

| Bound | Default | Behaviour on breach |
|---|---|---|
| Max tool calls | 10 | Loop stops, writing proceeds with what was gathered |
| Wall clock | 5 min | `context` deadline cancels mid-call |
| Token budget | task-specific | Loop stops, run marked `budget_exhausted` |
| Tool allowlist | read-only data tools | Unknown tool name → error returned to model, counted as a call |
| Repeated identical call | 2 | Short-circuited from cache, doesn't count against budget |

A breach is not a failure — it's a normal exit. The manifest is whatever was gathered, and the
validator judges the result on its own merits.

**As built (milestone 8).** Calls (default 4) and tokens (default 16,000) are `agent.Budget`; wall
clock is `-timeout`, the run's context deadline. Tokens are Ollama's `prompt_eval_count` plus
`eval_count` for every model call: the whole history is processed again each turn, so this is the
work the GPU does, not the size of the conversation. A question about one calendar used 1,800–3,500
tokens across both phases. A reply that takes research to its token budget has its tool requests
dropped, not run. The writer is told when research stopped early. The run records which budget ran
out (`runs.exhausted`). Not built yet: the cache for repeated identical calls, and per-task budgets
from config.

### 3.3 Provenance

The hard part, and the most interesting piece of engineering here.

Each run accumulates a **manifest**: an ordered list of `{tool, args, response, timestamp}` records.
After generation, the validator:

1. Extracts every numeric token from the draft (currency amounts, percentages, ratios, dates, counts).
2. Normalises them (thousands separators, currency symbols, decimal commas vs points, `1.2M` forms).
3. Checks each against the set of values present in the manifest's responses.
4. Rejects the draft if any token is unaccounted for.

**Hard case — derived figures.** "Yield rose from 3.1% to 3.4%, a 0.3pp increase" — the `0.3` is
correct but appears in no tool response. Resolution: **ban arithmetic in the writing prompt and
supply deterministic calculator tools** (`pct_change`, `diff`, `sum`) that the research loop calls, so
derived numbers enter the manifest legitimately. The tools are trivial to write, trivial to test, and
keep the invariant total. Fallback if this proves too strict in practice: downgrade unaccounted
numbers to `needs_close_review` rather than rejecting.

The calculators return **full `float64` precision and never round**. 1.50 → 1.55 is
`3.333333333333336`, and that is the value the manifest records and the data block carries. Rounding
is presentation, so the template does it — which means the figure the validator compares is exactly
the figure the tool returned, with no rounding rule duplicated between Go and Elixir. A calculator
also refuses inputs it can't answer honestly: `pct_change` from a zero or negative base returns an
error rather than `Inf` or a percentage whose sign reads backwards. Implemented in
[`internal/tools`](../internal/tools/calc.go), milestone 1.

**Hard case — false positives.** Years, list positions, "top 10", version numbers. Needs a small
ignore-list and a notion of "numbers that are not claims". Table-driven tests will earn their keep.

**Solved by the format contract.** Numbers live in typed frontmatter fields, not in prose (see
[rendering](rendering.md)), so validation is field-by-field comparison against the manifest rather
than regex extraction — and the prose is validated by the simpler assertion that it contains *no*
unaccounted numerics at all. This also removes the locale number-format problem entirely: figures
never appear in translated text.

**As built (milestone 6, [`internal/provenance`](../internal/provenance/)).** The manifest indexes
every number, date and string in a run's successful tool results, with the path each came from.
`CheckData` holds a post's data block to it field by field, strings included, so an invented ticker
fails like a rounded yield. `CheckProse` covers free text such as `agent -research` answers: it finds
numbers (currency, percent, thousands separators), dates (ISO, "Oct 8", "8 October 2026") and bare
years, and reports any the manifest lacks. Matching is exact: a rounded, converted or derived figure is
reported. The false positives above are handled by rule: digits inside words (`Q3`, `W38`) and list
positions at a line start aren't claims, and a bare year counts if a returned date falls in it. Known
limits, acceptable because post prose must have no figures at all (`NoFigures`): only English number
formats are parsed. *(Updated after milestone 8: numbers written as words, two to ninety-nine, are
figures too, and "October 16 and 17" reads as two dates. Besides returned values, the manifest
accepts each list's length, so "nine stocks" checks against a nine-item calendar, and the figures of
the question and of the date the run started, which the writer is told; see the
[measurement](benchmarks/2026-10-07-writer/README.md).)* In real runs, five of six research
answers traced fully; the sixth said "the next 4 months", a figure the model derived itself, which is
exactly what N1 forbids.

### 3.4 Retrieval (RAG)

Retrieval does not make the model *learn* — the weights never change; it puts relevant text in the
context window at call time. Framed honestly, it earns its place in three specific spots:

1. **Style memory.** Retrieve past **approved** drafts so new output matches the house voice. The
   review queue *is* the corpus, so every human approval improves the next draft. This is the closest
   honest version of "the agent gets better at what it does", and it's a real feedback loop.
2. **Don't-repeat-yourself.** Retrieve what was recently written about a ticker so week 4's digest
   doesn't rehash week 1's angle.
3. **Filings and news text.** Chunked 10-K and press-release text makes valuation write-ups richer.
   Where that text comes from is [open question 11](#5-open-questions): trusted sources only, never
   open browsing.

**The hard line: never retrieve a number.** Retrieved text is quotable as *language*; every figure
still comes from a live tool call. Vector stores have no freshness guarantee, and this is precisely
the crack hallucination would get back in through. Retrieved chunks enter the prompt but **not** the
provenance manifest — they can't authorise a number.

**Implementation: no vector database.** Embeddings stored as BLOBs in SQLite, brute-force cosine
similarity over `[]float32` in Go. At this corpus size — hundreds to low thousands of chunks — that's
microseconds and zero dependencies. It is the correct engineering choice at this scale, not a
shortcut; revisit only if the corpus grows two orders of magnitude. Embeddings from Ollama
(`nomic-embed-text` or a Qwen3 embedding model) alongside the chat model.

### 3.5 Translation

All seven Quantic locales are in scope from the first content milestone. The review burden is the
thing to design around: **the human reviews the source; translations are machine-verified, not
human-reviewed — with one exception.**

**English is the source.** It's Quantic's reference language and the canonical host, so the `en` draft
is what the validator gates and what the human reads first.

**Spanish is spot-checked.** `es` is the locale the reviewer can actually judge for correctness, so it
surfaces in the review queue alongside the source rather than publishing on machine verification
alone. It's a fluency check on the translation pass, not a re-review of the facts — those are already
guaranteed identical by the byte-identical `data` block.

The remaining five (`ca`, `fr`, `de`, `it`, `pt`) publish on machine verification. A sustained
pattern of problems found in the `es` spot-check is evidence the translation prompt is wrong for all
of them, so the queue records spot-check outcomes as a signal, not just a gate.

```
en draft → provenance ✓ → human review ✓ → canonical (reference)
                                │
                                ├→ es → translation validator ✓ → human spot-check → publish
                                │                                                      ↘ hold
                                └→ ca fr de it pt → translation validator ✓ ──────────→ publish
                                                                            ↘ hold
```

Only prose is translated. The `data` block — every figure in the post — is identical across all
seven locale files, and Phoenix formats numbers at render time using the app's existing localisation
(see [rendering](rendering.md)). The translation validator therefore asserts, per locale:

- **`data` block byte-identical to the source.** No figure can drift in translation because no figure
  is translated.
- **No numerics introduced into prose.** Same rule as the source draft.
- **Same structure** — prose section count and keys, link count and targets.
- **No added claims** — length within a tolerance band of the source.

This is a stronger guarantee than the locale-aware numeric normalisation it replaces, and much less
code.

Any mismatch holds that locale only; the source and the passing locales still publish. A held locale
surfaces in the review queue with the specific assertion that failed.

Note this is a *different mechanism* from Quantic's gettext workflow. Content is per-locale markdown
files, not msgids — the existing "never edit msgids, fill msgstr" rules don't apply here. The register
conventions do: informal throughout, Brazilian Portuguese for `pt`.

Quantic resolves locale from the **request host**, not a path prefix, so a post's seven files are
keyed by locale and served on the host that carries that language — see
[rendering](rendering.md#locales-are-hosts-not-paths).

### 3.6 Concurrency model

The interesting shape: **one GPU, many network calls.**

- LLM inference is a serialised resource — a semaphore of capacity 1 (configurable) around the model
  client. Queued tasks wait; they do not thrash the GPU. This matters more with an agentic loop,
  since one task now makes many sequential model calls.
- Tool fetches within a research step fan out via `errgroup`, bounded so Quantic isn't hammered.
- A `context` per run carries the deadline and cancellation, threading through both phases.
- Translation of the six non-source locales is embarrassingly parallel in principle but serialised in
  practice by the same GPU semaphore — a good place to learn what a bottleneck actually costs.
- The daemon is opportunistic: the machine is not always on. State is checkpointed in SQLite after
  each phase so an interrupted run resumes rather than restarts.
- **The GPU is shared, not owned.** The desktop is also the author's machine, and sometimes the author
  needs most of the VRAM. Both the agent and Ollama must be easy to stop, start and restart, and neither
  may lose work when stopped. So: `SIGTERM` is a clean stop at the next checkpoint, and an unreachable
  Ollama is a reason to wait, not to fail — the run stays queued until the server is back. The
  client reports that case as `llm.ErrUnavailable`, distinct from every other failure, and the
  agent exits with status 3 for it so a scheduler knows a retry is safe. `SIGTERM` (or Ctrl-C)
  cancels the request in flight and exits with 130; Ollama stops generating within about a second,
  so stopping the agent gives the GPU back straight away. The runbook has the commands ([target machine §8](target-machine.md#9-freeing-the-gpu)).
- **As built (milestone 8).** A run is checkpointed in SQLite when research ends (`runs.phase`:
  `research` → `write` → `done`), and every tool call is already recorded as it completes.
  `agent -resume N` continues a run that was interrupted or failed before answering: one stopped while
  writing calls no tool again; one stopped during research replays its recorded calls to the model
  as the conversation so far, and carries on within what's left of its budget. Claiming a run to
  resume is one conditional `UPDATE`, so two resumes of the same run can't both get it. Quantic's
  `429` is retried in `internal/mcp` with exponential backoff and jitter, long enough to outlast its
  one-minute window, then reported as `mcp.ErrRateLimited` (exit 3, resumable). Research with no
  successful call skips writing and ends `no_data` (exit 5, resumable): an answer about nothing isn't
  an answer. Adding that state rebuilt `runs` (migration `0003`), with foreign keys off for that one
  connection, as SQLite's own recipe requires. There is no
  scheduler yet: resuming is a command, not automatic.

### 3.7 Storage

SQLite via `modernc.org/sqlite` (pure Go, no cgo — preserves the static-binary property).

- `runs` — id, task_kind, params, phase, state, budgets_used, started_at, finished_at, error
- `tool_calls` — id, run_id, tool, args, response, duration_ms, called_at *(the audit log, N3)*
- `drafts` — id, run_id, locale, role (`source` | `spot_check` | `auto`), content, state,
  validator_report, created_at
- `manifests` — draft_id → tool_call_ids *(provenance link, N1)*
- `embeddings` — id, source_kind, source_id, chunk, vector BLOB, model, created_at
- `reviews` — draft_id, verdict, reviewer_note, decided_at *(feeds both style memory and the
  accept-rate metric)*

Retention: `tool_calls` responses can be large; plan a compaction policy before it becomes a problem.

**As built (milestone 7, [`internal/store`](../internal/store/)).** `runs`, `tool_calls` and `drafts`
exist; the draft's `validator_report` is its list of provenance findings. `manifests` isn't needed yet:
a draft is checked against all its run's successful calls, so the run *is* the link. It arrives when one
run produces several drafts. Migrations are numbered SQL files embedded in the binary, each applied once
in its own transaction. Tool calls are written as they happen, so a stopped or crashed run keeps its
audit trail, and `agent -run N` re-checks a stored answer against exactly the results it was given,
whatever the tools would say today. The database lives at `$XDG_STATE_HOME/quantic-agent/agent.db`
(`~/.local/state/...` by default); `-db` or `QUANTIC_AGENT_DB` override it.

**Migrations are hand-written, on purpose, with a known gap.** Go's standard library has no migration
tool; the usual choices are `pressly/goose` or `golang-migrate/migrate`. For one process, SQLite and
forward-only migrations, forty lines in `internal/store` do the job without a dependency. What they
lack: down migrations, a CLI, and **locking across processes**. Two processes opening a *new* database
at the same moment can collide: in a test of 20 simultaneous pairs, 2 of the 40 opens failed with
`database is locked (SQLITE_BUSY)` while creating `schema_migrations`. Nothing is lost, and the next
start succeeds, but that process exits with an error. Milestone 9's worker pool is one process, so it's
unaffected.

**Decision: switch to goose at the start of milestone 8.** A migration library is what most Go
applications with a SQL database use (`golang-migrate/migrate` and `pressly/goose` are the two usual
ones), and doing it by hand first, then with the library, shows both approaches. Goose brings what the
hand-written version lacks: down migrations to undo a migration that turned out wrong, locking across
processes, and a CLI. It reads SQL migrations from an `embed.FS`, so `0001` carries over, gaining the
`-- +goose Up` / `-- +goose Down` markers.

**As built (milestone 8).** "Locking across processes" was wrong for SQLite: goose ships lockers for Postgres and MySQL only, whose servers offer a lock to take. Measured
with real processes, goose without a lock collided more often than the hand-written code (11–13 of
40 opens of a new database failed, against 6–7). `internal/store` takes an exclusive `flock` on
`agent.db.lock` around migrating, which made it 0 of 200, and also covers the one-time handover of a
milestone 7 database: its versions move from `schema_migrations` into goose's `goose_db_version`, in
one transaction. A test takes every migration down and back up. `0002` adds `phase`, `tokens` and
`exhausted` to `runs`.

### 3.8 Delivery

- **Review queue** — SQLite rows plus a minimal read-only HTTP view (`net/http` + `html/template`,
  no JS framework) once there are more than a handful of drafts. Deliberately not a product.
- **Pull requests** — `go-github`: branch, commit one file per locale (typed frontmatter + prose,
  per the [format contract](rendering.md)), open PR against the private Quantic repo targeting
  `/insights`. Token scoped to branch and PR creation only; `main` is protected independently, so a
  bug in the agent cannot merge. NimblePublisher compiles posts at build time, so a malformed post
  fails CI rather than a request.
- **Issues** — for data-QA findings with no mechanical fix.

## 4. Stack

| Concern | Choice | Reasoning |
|---|---|---|
| Inference server | Ollama | Three model tiers behind one endpoint with load/unload on demand, embeddings on the same server, and per-model capability metadata. `llama.cpp` stays the escape hatch ([llm-kit](https://github.com/fleveque/llm-kit)) with explicit triggers — [decision 0004](decisions/0004-ollama-now-llama-cpp-on-a-trigger.md). |
| Default model | Qwen3.5-9B, Q4_K_M (6.6GB) | Measured on the target card: 100% on GPU up to 64K context (8.0GB), about 80 generated and 4,400–5,000 prompt tokens/second — [decision 0005](decisions/0005-default-model-by-measurement.md). |
| Candidates | `qwen3.6:35b` (MoE); Qwen3.8-27B `UD-IQ3_S` | The MoE writes at ~70 tokens/second even with 44% of itself in system RAM; the dense 27B fits fully only at 8K. Decided on task quality from milestone 5, not on speed or size. |
| Fast model | Qwen3.5-4B (3.4GB) | Mechanical passes — data-QA triage, classification — where a 9B is overkill. |
| Embeddings | `nomic-embed-text` via Ollama | Small, fast, good enough for a few thousand chunks. |
| Model I/O | JSON-schema-constrained decoding | Local models are flakier at native tool-calling than frontier models; constrained output is the reliable floor, native tool-calling an optimisation. |
| Tool schemas | Generated from Go structs by reflection | Single source of truth; the Go type *is* the schema. |
| Persistence | `modernc.org/sqlite` | Pure Go, no cgo. |
| GitHub | `go-github` | Mature, well-typed. |
| Logging | `log/slog` | Stdlib structured logging; feeds N3. |
| Config | YAML + env, flags for overrides | Secrets via env only, never committed. |

**Thinking models.** The Qwen3.5-era models run a reasoning pass before answering, returned in a
separate `thinking` field. Both phases send `think:false`. Reasoning text is not a source: it never
enters the manifest, never reaches a draft, and is not something N1 could validate even in principle.
It is also expensive — a thinking reply spends its token budget on reasoning first, so a truncated
one can arrive with a full `thinking` field and an empty answer. Requests carry `think:false`
explicitly rather than relying on a default, and a model with no thinking mode accepts the field and
ignores it.

**Hardware:** 64GB RAM, RTX 4070 Ti Super (16GB VRAM).

**KV cache before quantisation.** At long research contexts the KV cache outgrows the weights, so
`OLLAMA_KV_CACHE_TYPE=q8_0` (about half the cache memory, needs flash attention) is the first lever to
pull before trading model size away. It fails quietly on architectures without flash attention
support, so it is a thing to verify on the target machine rather than assume.

**Sizing the model to the card** — the reasoning before measurement; the measured outcome is in
[decision 0005](decisions/0005-default-model-by-measurement.md). The newest Qwen at this size is **Qwen3.8-27B**. Ollama's own library
publishes it only at Q4_K_M (18GB), which cannot hold weights *and* a KV cache in 16GB. But Unsloth
publishes sub-4-bit GGUFs on Hugging Face, which Ollama pulls directly
([decision 0004](decisions/0004-ollama-now-llama-cpp-on-a-trigger.md)): `UD-IQ3_S` is 12.0GB,
`UD-Q3_K_XL` 13.1GB, `UD-IQ4_XS` 14.3GB.

What makes the 27B viable at these sizes is its architecture: 64 layers arranged as 16 × (3 Gated
DeltaNet + 1 standard attention), with 4 KV heads on the attention layers. Only those 16 layers keep a
KV cache — roughly a quarter of what a conventional 64-layer model needs at the same context — and the
DeltaNet state doesn't grow with context at all.

The risk is quality, not fit. Below 4 bits per weight a model degrades, and the degradation shows up
first in exactly what this agent leans on: precise structured output and tool-call arguments. A
throughput benchmark can't see that. So the 27B is the *primary candidate*, confirmed by `cmd/bench` on
the target card for speed and residency, and by tool-call validity once milestone 5 exists. The 9B
remains the default until both say otherwise — a default that silently fails to fit is worse than a
smaller one that works.

An earlier version of this section concluded that no 27B fits in 16GB. That was true of Ollama's
library and false once Hugging Face quants were counted — the research had only looked at models
already installed on the development laptop.

**Development is not deployment.** The agent is written on one machine and runs on another, so no
model name, host or path is baked into the binary. `-model` / `QUANTIC_MODEL` and `-ollama` /
`OLLAMA_HOST` carry them ([runbook](target-machine.md)), and `agent -check` prints what the server it
is pointed at actually has,
including each model's `capabilities` — the `tools` entry is what milestone 5 depends on.

Keep the target's Ollama current. Version floors per model are undocumented, and the desktop's 0.24.0
refused Qwen3.5 outright with a `412: requires a newer version of Ollama`.

## 5. Open questions

1. **MCP authentication from a headless daemon.** Quantic's MCP server uses OAuth, which assumes an
   interactive consent flow. A local daemon needs a non-interactive credential: a long-lived service
   token scoped to read-only tools, a direct internal API path, or a one-time grant with a stored
   refresh token. *Answered 2026-10-06* — [decision 0006](decisions/0006-anonymous-mcp-for-public-tools.md):
   the server already answers anonymous callers for its public reference tools and refuses them its
   portfolio tools, so the agent connects anonymously. A service token, with no user behind it, is
   the plan for when an authenticated reference tool is needed.
2. **The `/insights` section doesn't exist yet.** It's Phoenix work in the private repo: route,
   layout, markdown rendering, per-locale routing consistent with the existing language subdomains,
   sitemap and hreflang, plus the free-registration gate and its `schema.org` flexible-sampling
   markup. The agent has nowhere to open PRs against until it exists. *Prerequisite for
   milestone 11.* The only agent-side obligation is emitting the `fold_after` marker in draft
   frontmatter — see the [content plan](content.md#registration-gate).
3. **How do we know a draft is any good?** Provenance proves it isn't *wrong*; it doesn't prove it's
   worth publishing. Track human accept/reject rate per task kind and treat it as the metric that
   matters. A task kind below some accept rate is broken — the task, not the reviewer.
4. **Idempotency.** Re-running after a crash must not produce a duplicate weekly digest. Natural key
   per run (kind + period) and an upsert.
5. **Scheduling.** Internal ticker in the daemon vs. systemd timers invoking a one-shot binary. The
   one-shot model is simpler and more crash-tolerant; the daemon model makes the worker pool
   meaningful. Probably both.
6. **Prompt storage.** Prompts are code and should be reviewed like code — versioned in the repo via
   `embed`, not stored in the database. They must contain no Quantic-internal content (N4).
7. **Chunking strategy for filings.** 10-Ks are long and structured. Naive fixed-size chunking will
   split tables badly. Deferred until milestone 10.
8. **Can a 14B model reliably write prose with no figures in it?** The format contract forbids
   numbers in prose entirely. Small models will violate this. The validator catches it and retries,
   but if the violation rate is high the writing prompt needs restructuring — possibly generating
   prose and data in separate calls. This is the likeliest place the accept-rate metric first bites.

9. **Which primary model, measured on the target box.** Qwen3.8-27B at `UD-IQ3_S` or `UD-Q3_K_XL`
   (best model that fits, reduced precision) against Qwen3.5-9B Q4_K_M (fits easily, most context).
   The deciding numbers are tokens/second and GPU residency at realistic research-context lengths —
   where the 27B's cache stops fitting — and, from milestone 5, tool-call validity at 3-point-something
   bits. Nothing in the code depends on the answer — it is one flag.

   `cmd/bench` measures it: prompt and generation rates per model and context size, plus the share of
   each model that stayed in VRAM (`/api/ps` reports `size` against `size_vram`, and anything below
   100% means layers spilled into system RAM). Run it on the deployment machine — a development
   laptop with no CUDA device reports 0% and about 10 tokens/second, which answers nothing about the
   4070 Ti Super. Two further notes the tool encodes: each measurement carries a unique nonce,
   because Ollama's prefix cache otherwise reports cached tokens as if it had processed them, and
   every request sets `num_ctx`, because the server defaults to a 4096-token window whatever the
   model supports and silently truncates a longer prompt.

   *Answered 2026-09-26 by measurement* — [decision 0005](decisions/0005-default-model-by-measurement.md).
   The 9B stays the default. The 27B fits entirely only at 8K, and a mixture-of-experts model
   (`qwen3.6:35b`) turned out to be the more interesting candidate: spilling into system RAM costs it
   little. Quality decides between them from milestone 5.

10. **Model upkeep as a task.** How the agent learns a new model exists (Ollama library, Hugging Face
    feeds, a hand-maintained list), and whether it may pull one itself. Today pulling is a deliberate
    manual step ([runbook §3](target-machine.md#3-pull-the-candidate-models-34gb)): models are
    6–50GB, and a scheduled pull would take disk space and the GPU without anyone asking. The likely
    answer is that the agent proposes candidates and a human pulls them, after which the agent runs the
    evaluation and opens the PR. Needs the milestone 5 evaluation set first.
11. **Where filing and news text comes from.** §3.4 plans to retrieve 10-K and press-release text for
    valuation write-ups, but nothing names the source. Open web browsing is ruled out: a page can carry
    instructions aimed at the model (prompt injection, which small local models resist poorly), a
    figure from an arbitrary page would look like a recorded tool call without being trustworthy
    (N1), and it would widen the research allowlist from a known set to the whole internet. The likely
    answer is a short list of trusted sources, each its own read-only tool in the allowlist — SEC
    EDGAR for filings, a company's own investor-relations releases — with fetched text treated as
    untrusted input: stored and embedded as language, never followed as instructions, never a source
    for a number. Open: which sources, whether the text comes from Quantic's MCP server rather than
    the agent fetching it, and how non-US issuers are covered. *Needed by milestone 10.*

## 6. Explicit non-goals

- Not a general-purpose agent framework. It does a handful of known jobs well.
- Not a product. No multi-user support, no auth beyond the local machine, no hosting.
- Not real-time. Everything here is batch work that tolerates minutes of latency.
- Not fine-tuning. Retrieval, not training. If the model needs to be better, use a better model.
- Not touching business logic, financial calculations, or anything involving real user money.
