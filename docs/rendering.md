# Content format and rendering

How a generated post becomes a page that looks like Quantic. The contract between what the agent
emits and what the Phoenix app renders.

---

## The problem with "the agent writes markdown"

Markdown can express prose. It cannot express the Quantic design system.

The Dividend Week Ahead is mostly *data*: a table of ex-dividend dates with per-row safety badges,
yield figures that want colour, ticker chips that link to `/stocks/:symbol`, raise percentages in
green and cuts in red. In markdown that's `| MSFT | 2026-09-18 | 0.83 |` — a grey generic table. The
components that would make it look like Quantic already exist in the app and markdown has no way to
reach them.

Emitting HTML from the agent is worse: unreviewable in a PR diff, an injection surface, and it welds
generated content to whatever the frontend looked like the day it was generated.

## It is still a markdown file

Worth stating plainly, because the section below can read as more exotic than it is: **the agent
commits a `.md` file, and Quantic does the markdown-to-HTML conversion.** That part is exactly the
obvious design.

The only refinement is *where the numbers live*. Both of these are `.md` files rendered by the app;
one of them can reach the design system and one can't.

## The format: structured data + named prose

A post is **one file per locale** with YAML frontmatter and a markdown body:

- **Frontmatter carries the data** — typed fields, not markdown tables.
- **The body carries only prose** — the paragraphs a human would actually write.
- **The layout is a Phoenix template per post kind**, not something the agent controls.

```yaml
---
kind: week_ahead
period: 2026-W38
published_at: 2026-09-13
locale: en
fold_after: summary          # registration gate splits here
manifest_id: 01JQ8F...       # provenance link

data:
  summary:
    ex_dividend_count: 23
    raises_count: 3
    cuts_count: 1
    at_risk_count: 2
  ex_dividends:
    - symbol: MSFT
      ex_date: 2026-09-18
      amount: 0.83
      currency: USD
      yield_pct: 0.71
      safety: strong
    - symbol: O
      ex_date: 2026-09-19
      amount: 0.2695
      currency: USD
      yield_pct: 5.42
      safety: watch
  raises:
    - symbol: ITW
      previous: 1.50
      current: 1.55
      change_pct: 3.333333333333336   # full precision; the template rounds
      currency: USD
  charts:
    - component: dividend_growth
      symbol: ITW
---

## What to watch

Several consumer-staples names go ex-dividend in the same week for the first time this
quarter, and the REIT cohort keeps its recent pattern of monthly payers holding steady
while quarterly payers drift.
```

The body has no numbers in it. That is a **rule**, not a coincidence — see below.

## Rendering

**Build-time compilation with [NimblePublisher](https://github.com/dashbit/nimble_publisher).** It
reads the markdown files at compile time, parses frontmatter, and produces structs — zero runtime
parsing cost, and a malformed post fails the *build* rather than a request. Since merging to `main`
already deploys to production, the flow is clean: PR merged → deploy → post live.

**Markdown engine: [MDEx](https://github.com/leandrocp/mdex)** (comrak via NIF) with raw HTML
disabled. Faster than Earmark, GFM tables and footnotes, and — since the input is model-generated —
the fact that unsafe HTML is off by default matters.

**Layout: a Phoenix template per `kind`.** `week_ahead.html.heex` knows the shape of a Week Ahead:
where the summary tiles go, that `data.ex_dividends` renders through `<.dividend_table>`, that each
row gets a `<.safety_badge>` and a `<.ticker_chip>`, that `data.charts` renders the existing SVG
chart components. The prose slots in by heading.

```
post.md ──build──► %Post{data: %{...}, prose: %{...}}
                          │
                          ▼
              week_ahead.html.heex
                          │
        ┌─────────────────┼──────────────────┐
        ▼                 ▼                  ▼
  <.stat_tiles>   <.dividend_table>    <.chart> (existing SVG components)
                    └ <.safety_badge> <.ticker_chip>
```

**The agent never controls presentation.** It supplies typed data and paragraphs. A redesign of
`/insights` touches templates only — no regenerating content, no rewriting old posts.

### Why not directives in the body?

An alternative is `{{ block: ex_dividends }}` markers in the markdown that a parser swaps for
components. Rejected: it needs a custom parser, it's an injection surface fed by model output, and it
lets the agent make layout decisions it has no basis for making. A fixed template per kind is
simpler, safer, and smaller to validate.

The cost is that a new post *shape* needs a new template. That's fine — there are four planned
formats, not four hundred.

### Why not have a human (or Claude Code) transform the markdown afterwards?

A tempting shortcut: let the agent emit plain prose markdown, then convert it to the structured form
as a second commit on the PR — by hand, or with a frontier model that's far better at the
transformation than a local 14B.

Rejected for the recurring case, for two reasons.

**It breaks provenance.** N1 says every published figure traces to a recorded tool call. The validated
artifact is the draft the validator passed. If an unvalidated step afterwards rewrites the content,
what ships is no longer what was checked, and a transposed digit introduced during transformation has
nothing to catch it. The guarantee has to hold all the way to the commit.

**It doesn't scale, and it's the wrong direction of effort.** This runs weekly across seven locales —
a per-post manual step is ~350 transformations a year. And the premise doesn't hold: emitting
frontmatter is *easier* for the agent than writing good prose tables, not harder. The data comes back
from the tools already structured; the Go side is `yaml.Marshal` on a struct it already has. Asking
the model to flatten structured data into a markdown table, so that something else can rebuild the
structure later, is strictly more work and more places to lose fidelity.

**Where a frontier model genuinely helps: once, on the templates.** Turning "here's what a Week Ahead
should look like" into `week_ahead.html.heex`, the `Post` struct, and the components around them is a
one-time job in the Quantic repo, and a good one to hand to Claude Code. After that the agent emits
frontmatter directly and no per-post transformation exists.

**Legitimate as scaffolding only.** Before the template exists, the first two or three posts can be
hand-rendered to validate that the format is right. That's a bootstrap with an end date, not the
architecture.

### Charts

`data.charts` names existing components by symbol. The SVG chart components already shipped on
`/stocks/:symbol` render server-side, stay theme-aware, and cost the agent nothing to "generate" —
it just names one. No image generation, no stale PNGs.

---

## Two problems this format removes

### 1. Provenance validation gets exact

When numbers live in prose, validating them means extracting numeric tokens with a regex and matching
them heuristically against tool output. When numbers live in typed frontmatter fields, validation is
**field-by-field comparison against the manifest** — `data.ex_dividends[0].amount` either equals what
`dividend_calendar` returned for MSFT or it doesn't. No regex, no normalisation guesswork, no false
positives on years and list positions.

This makes the prose rule enforceable and cheap: **prose must contain no figures at all.** The
validator asserts zero unaccounted numerics in the body, and every figure is checked structurally.
A model that writes "yields rose about 40 basis points" in the body fails validation and gets sent
back. Numbers in words are figures too: "three names" is a count nothing in the prose can verify,
exactly like "3 names". A count worth stating goes in the data block.

### 2. Locale number formatting disappears

The [translation design](design.md#35-translation) worried about `3,400.50` vs `3.400,50` — the
validator having to normalise per locale before comparing numeric multisets. That problem is gone.

Numbers never appear in translated text. They live in the data block, identical across all seven
locale files, and **Phoenix formats them at render time** using the app's existing localisation. The
translation pass only ever touches prose.

So the translation validator simplifies to: same structure, same prose sections, no numbers
introduced, length within tolerance. The `data` block is asserted byte-identical to the source, which
is a much stronger guarantee than the one it replaces.

---

## Locales are hosts, not paths

Quantic resolves locale from the **request host**, app-wide — `quantic.finance` is `en` and
canonical, `quantic.es` is `es`, `quantic.cat` is `ca`, and `pt`/`de`/`fr`/`it` live on
`{lang}.quantic.finance`. There are no path prefixes, which is why internal links (`~p"/…"`) are
locale-agnostic.

Three consequences for content, all of them simplifications:

- **The seven files are keyed by locale, not by path.** `/insights/week-ahead-2026-W38` is the same
  route on every host; which locale's file it renders is decided by the host that served the request.
- **Canonical, hreflang and per-host sitemaps are already automatic** through the existing SEO layer,
  driven by `:locale_origins`. The section needs to appear in the sitemap; the seven-way alternate
  wiring is not new work.
- **Links inside a post need no locale handling.** A ticker chip pointing at `/stocks/MSFT` resolves
  on whatever host the reader is on, so the agent emits plain paths and never thinks about locale.

## What this needs from the Quantic repo

Prerequisite work, none of it agent work:

1. `/insights` route and index, plus the section registered in the sitemap. No locale-aware routing
   needed — the host already carries it.
2. NimblePublisher + MDEx wired up, raw HTML disabled, one `Post` struct per kind, locale-keyed
   lookup.
3. `week_ahead.html.heex` and the components it composes (most already exist).
4. The free-registration gate honouring `fold_after`, with `schema.org` flexible-sampling markup.
