# Lesson 06 — Every figure has a source

**Milestone 6** — the agent now checks its own answers. Every number and date it writes must be one a
tool returned; if one isn't, the answer is shown with the culprits listed and the run fails. This is the
rule the whole project is built around (N1), and the first thing it caught was a lie in my own tests.

*Also readable as a [formatted page](https://claude.ai/artifact/VneRrzy5zK36bKXuX48uzb). For the code itself, file by file, see the
[walkthrough](https://claude.ai/artifact/PJ7YDwFzobrQMCoKEB5UJ7).*

---

## Where milestone 5 left off

One run of `agent -research "Which stocks go ex-dividend in the next six months?"` got 120 days of
calendar from Quantic and answered:

```
...here are the companies that have ex-dividend dates within the next 180 days...
```

"180" is in that answer, and in no tool result. Nothing stopped it. Milestone 6 is what stops it.

## Two checks, because there are two kinds of output

The design (§3.3) wanted to extract every number from a draft and match it against tool output, and
admitted that's the hard part: currency symbols, thousands separators, years that aren't claims, list
numbers. Then the post format ([rendering](../rendering.md)) made most of it unnecessary: a post keeps
its figures in typed fields and its prose free of figures entirely. So the validator has three entry
points:

- **`CheckData`** for a post's typed data: every value must be one a tool returned, compared field by
  field. No parsing, no guessing.
- **`NoFigures`** for a post's prose: there must be none at all.
- **`CheckProse`** for free text that does contain figures, like a `-research` answer: extract them,
  then check each one.

The first two are the ones the published posts will rely on. The third is the hard one, and it's what
caught "180".

## The manifest: an index of everything the tools said

Each successful tool result is JSON, so `NewManifest` decodes it into `any` and walks it with a type
switch:

```go
switch v := v.(type) {
case float64:
	m.numbers[v] = append(m.numbers[v], at)
case string:
	m.strings[v] = append(m.strings[v], at)
	...
case []any:  // recurse into each element
case map[string]any:  // recurse into each value
}
```

Decoded into `any`, JSON only ever produces six kinds of value: `float64`, `string`, `bool`, `nil`,
`[]any` and `map[string]any`. That makes the walk complete with four cases. Each value is stored with
where it came from, like `dividend_calendar#0 $.stocks[2].ex_dividend_date`, so a finding can point
somewhere.

A `float64` as a map key felt wrong to me at first, because "never compare floats with `==`" is drilled
into everyone. Here exact comparison is the point. A tool says `0.2695`; if the answer says `0.2695`,
both are parsed by the same `strconv.ParseFloat` rules into the same bits, and the lookup finds it. If
the answer says `0.27`, it's a different number, and it *should* be reported: rounding is presentation,
and presentation belongs to the template.

## Finding figures in prose

`CheckProse` finds dates first, then numbers, and the order matters. Without it, "Oct 8" is a correct
date *and* a bare "8" that no tool returned. So every date is blanked out of the text before numbers
are searched:

```go
masked := []byte(text)
...
blank(loc[0], loc[1]) // after recording each date
```

Breaking that is one of the walkthrough's experiments: every day-of-month in every test starts failing.

Then the false positives the design predicted, each answered by a rule with a test:

| Text | Why it isn't a claim |
|---|---|
| `Q3`, `W38`, `3M`, `qwen3.5`, `COVID-19` | Digits glued to letters are part of a word |
| `1. Realty Income` at a line start | A list position |
| `the 2026 calendar` | A bare year counts if any returned date falls in it |

The checks for "glued to a letter" use the `unicode` package (`unicode.IsLetter`), not byte
comparisons. A company name can start with "É", and a byte isn't a character.

## Exact means strict

Six real runs, three questions:

```
=== Which companies go ex-dividend in the next 10 days? (run 1)       exit 0
=== Which companies go ex-dividend in the next 10 days? (run 2)       exit 0
=== Is Microsoft going ex-dividend this month? ... (run 1)            exit 0
=== Is Microsoft going ex-dividend this month? ... (run 2)            exit 0
=== Which stocks go ex-dividend in the next six months? (run 1)       exit 4
agent: 1 figure(s) in the answer came from no tool result:
  "4" (number 4)
=== Which stocks go ex-dividend in the next six months? (run 2)       exit 0
```

Five answers traced completely, every date included. The sixth said "the next 4 months": the model
divided 120 days by 30 itself. A human would let that pass. N1 doesn't, and shouldn't: today it's a
harmless conversion, tomorrow it's "yield up about 0.3%" calculated in the model's head. The design's
answer is the calculator tools from milestone 1, which put derived figures into the manifest
legitimately. Exit status 4 is new: "this answer contains figures no tool returned".

## The first thing it caught was my test

I wired the check into `agent -research` and ran the existing tests. One failed (its stderr unpacked
onto separate lines here; Go prints it as one quoted string):

```
--- FAIL: TestRunResearch (0.00s)
    exit code = 4, want 0
    agent: 7 figure(s) in the answer came from no tool result:
      "October 6, 2026" (date 2026-10-06)
      "Oct 6" (date --10-06)
      "Oct 8" (date --10-08)
      ...
```

The test replays a real model answer listing five companies and their dates. Its fake Quantic server,
written in milestone 5, returned an *empty* calendar: `{"days":10,"stocks":[]}`. So the test had been
asserting that an answer full of dates was fine, when the tool it was "based on" contained none of them.
Exactly the situation N1 exists for, and I'd built it into a test without noticing.

The fix was to make the fake serve the real captured calendar. The broken version became a test of its
own: an answer whose data didn't come back from the tool must exit 4.

## Map order is random, on purpose

`CheckData` walks a post's data block and reports findings with their path, like
`ex_dividends[1].ex_date = 2026-10-07`. For a map, it sorts the keys first. Go randomises map iteration
order deliberately, so programs can't come to depend on it. Without the sort, the test with two findings
in a map failed in 6 runs out of 20: the findings were right, their order wasn't. A user reading two
reports of the same draft in different orders would rightly wonder what else is unstable.

## Table-driven, on real data

The prose test is one table of 25 rows: a name, a manifest, a sentence, and the figures it should
report.

```go
{"the claim from lesson 05", cal, "here are the companies that have ex-dividend dates within the next 180 days", []string{"180"}},
{"the window the data covers", cal, "Here are the stocks going ex-dividend in the next 120 days", nil},
{"a derived figure", cal, "in the next 4 months", []string{"4"}},
```

`cal` is the real 120-day calendar from quantic.finance, the same data behind lesson 05's answer. A
separate test feeds the validator the real answer captured in milestone 5, with the real data it was
written from, and expects no findings. Adding a case is one line, which is what makes the false-positive
rules cheap to pin down.

## What it doesn't do

Numbers written as words ("five companies") aren't seen, and only English number formats are parsed.
Both are acceptable for the same reason: the posts' prose may contain no figures at all, and the posts'
figures live in typed fields that are checked exactly. `CheckProse` is for the agent's working answers;
it's strict where it can be and honest about where it can't.

## What I'm taking into milestone 7

- Match exactly. A rounded, converted or derived figure is a different figure; derived ones come from
  calculator tools.
- Decoded into `any`, JSON is six types; a type switch handles all of them.
- Order matters when one pattern contains another: find dates, blank them, then find numbers.
- Never rely on map order. Sort keys when output order matters, and a repeated test run shows it.
- A validator applied to your own fixtures is a test of the fixtures too.
- Milestone 7 stores runs and their tool calls in SQLite. The manifest is rebuilt from those records, so
  any answer can be re-checked later against exactly what the agent saw (N3).

---

**Previous:** [Lesson 05 — The first real tool](05-the-first-real-tool.md) ·
**Next:** [Lesson 07 — A memory that can be audited](07-a-memory-that-can-be-audited.md)
