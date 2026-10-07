# The writing phase, measured — 2026-10-07

Milestone 8 split one research loop into two phases: research (the model calls tools), then writing
(a separate model call with no tools, given only the question and the data). These are real runs of
`agent -research`, `qwen3.5:9b`, live data from quantic.finance, on the target machine. Three
questions, three runs each, per version, all within four minutes, so every version saw the same
calendar.

| Version | Writer sees | Traced (exit 0) | What went wrong |
|---|---|---|---|
| [Milestone 7, one loop](m7-single-loop.txt) | (no separate writer) | 6/9 | "16 and 17" (×2: the bare day isn't read as a date); a derived end date, February 4, 2027; "four months" in words, not caught |
| [**Milestone 8, as built**](m8-writer-data-only.txt) | question and data | 6/9 | a derived end date; "current date (October 2023)"; an invented December 31, 2026; "ten" stocks in three answers (there are nine), not caught; one answer repeats its instructions |
| [Without the "don't derive figures" sentence](m8-writer-data-only-shorter-prompt.txt) | question and data | 7/9 | wrong counts in words in three answers ("six", "three", "five" companies; four listed); unsure what "this month" means twice; research ended with no data twice |
| [Writer sees the research conversation](m8-writer-sees-research-conversation.txt) | the research messages, no tools | 6/9 | **buy-timing advice twice** ("you must purchase the stock before the ex-dividend date": design N5); "18 days"; research ended with no data once, and the answer suggested Bloomberg |

**Follow-up, the same day:** the writer told today's date, and the validator reading numbers in words,
with list lengths and the question's own figures as sources ([runs](m8-writer-with-todays-date-and-word-numbers.txt)):
**7/9** traced. Every answer worked from 2026-10-07; none guessed a year. The two flagged answers
were caught by the new checks: "9 days away" (derived), and "four" and "two" companies, right but
counts of a filtered subset, which no list length accounts for. Not caught, because no figure is
invented: one answer put Apple, 9 days out, outside a 10-day window. One run's research made no call
at all and still ended answered.

Tokens per run (both phases): 1,827–3,500, median about 2,000. The default token budget, 16,000,
is about five times that.

## What this says

- **No version is clearly better on provenance.** 6, 6, 7 and 6 of 9. Three runs per question can't
  separate them; their failures differ more than their scores.
- **The writer doesn't know what day it is.** Nothing tells it; it has to infer "today" from the
  calendar's `from` field, and sometimes doesn't ("October 2023"). The single loop inferred it more
  often, but wasn't told either.
- **Numbers in words escape the validator.** "ten stocks" when the data has nine is an invented
  figure (N1), and `CheckProse` only reads digits. This is the most serious finding, and it isn't
  the writer's: milestone 7's "four months" slipped through the same way.
- **Research sometimes gathers nothing.** The model answers without calling a tool, or gives up after
  its 180-day request is refused. The writer then says, correctly, that there's no data, and the run
  counts as answered.
- **Showing the writer the research conversation led to buy-timing advice.** It reads most like
  milestone 7's answers, and twice told the reader to buy before the ex-dividend date: the very
  sentence [decision 0006](../../decisions/0006-anonymous-mcp-for-public-tools.md) removed from the
  tool's description. Neither milestone 7 nor the as-designed writer (data only) did, in these runs.

Milestone 8 keeps the writer the design specifies ([decision 0001](../../decisions/0001-agentic-research-constrained-writing.md)):
question and data only. The findings above are follow-ups, listed in CLAUDE.md.

The [interrupted-and-resumed run](../../lessons/08-a-loop-that-can-stop.md#the-payoff) in lesson 08
was made with the as-built version.
