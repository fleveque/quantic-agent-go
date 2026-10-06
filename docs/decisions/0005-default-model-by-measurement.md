# 0005 — Keep the 9B as default; choose models by measurement, repeatedly

**Status:** accepted · **Date:** 2026-09-26 · **Revisit at:** milestone 5 (first quality numbers), and
whenever a new candidate model is published

## Context

Design [open question 9](../design.md#5-open-questions) asked whether Qwen3.8-27B at ~3.5 bits could
replace `qwen3.5:9b` as the default on the target machine: RTX 4070 Ti Super (16GB), 64GB RAM,
Ollama 0.34.4. The desktop itself holds about 1.2GB of VRAM (Hyprland, a browser, Steam) whenever the
agent runs, so that was left in place: it's the real operating condition.

`agent -check` first: all three candidates report `tools` and `thinking`, including the two `hf.co`
pulls, whose capabilities come from the GGUF's chat template rather than Ollama's library. So the 27B's
tool calling isn't blocked on packaging.

## What the benchmark showed

[`2026-09-24-bench.json`](../benchmarks/2026-09-24-bench.json), f16 KV cache:

| Model | Context | Prompt tok/s | Gen tok/s | Size | On GPU |
|---|---|---|---|---|---|
| qwen3.5:9b | 32K | 4979 | 85.4 | 6.6 GB | 100% |
| qwen3.5:9b | 64K | 4437 | 77.2 | 8.0 GB | 100% |
| Qwen3.8-27B `UD-IQ3_S` | 8K | 1610 | 18.4 | 12.2 GB | 100% |
| Qwen3.8-27B `UD-IQ3_S` | 32K | 1323 | 21.2 | 14.6 GB | 91% |
| Qwen3.8-27B `UD-IQ3_S` | 64K | 914 | 6.1 | 16.9 GB | 77% |
| Qwen3.8-27B `UD-Q3_K_XL` | 32K | 1118 | 13.7 | 15.8 GB | 84% |
| Qwen3.8-27B `UD-Q3_K_XL` | 64K | 831 | 4.6 | 18.2 GB | 71% |

The 27B fits entirely only at 8K. Part of the reason is one the design didn't count: the `hf.co` pulls
ship a vision projector, and Ollama reserves about 1.16GB of VRAM for it (`llama-server` log:
"estimated worst-case memory usage of mmproj is 1161.02 MiB"), although the agent never sends images.
Rebuilding the model without it did make the 27B fit at 32K, but that means maintaining a hand-built
variant of every model, and new models arrive too often for that. We use models as published.

**Dense vs mixture-of-experts.** A dense model reads every weight for every generated token, so
whatever spills into system RAM (DDR5, roughly a tenth of the card's bandwidth) sets the pace. A
mixture-of-experts model reads only the experts it routes to. Two stock models already on the machine
([`2026-09-26-moe-vs-dense.json`](../benchmarks/2026-09-26-moe-vs-dense.json)):

| Model | Context | Prompt tok/s | Gen tok/s | Size | On GPU |
|---|---|---|---|---|---|
| qwen3.6:27b (dense) | 32K | 902 | 6.4 | 19.1 GB | 68% |
| qwen3.6:35b (MoE, 8 of 256 experts) | 32K | 1259 | 73.2 | 23.9 GB | 56% |

With 44% of itself in RAM, the MoE still writes at over 70 tokens/second. The 64GB of RAM is a real
resource for MoE models and close to useless for dense ones.

## Decision

1. **`qwen3.5:9b` stays the default.** It fits at every measured context with more than 7GB to spare,
   reads prompts 3–5× faster than any alternative (which matters to a research loop that rereads a
   growing manifest), and leaves the most GPU free. `defaultModel` doesn't change.
2. **Candidates:** `qwen3.6:35b` (MoE, fast despite spilling) and Qwen3.8-27B `UD-IQ3_S` (newest,
   usable to about 32K). Neither replaces the default on speed alone; what decides is quality on this
   agent's tasks, which nothing measures before milestone 5.
3. **Size and release date don't decide.** Newer models usually do more per parameter, and an MoE's
   total size says little about how it behaves. The deciding measurement is on our tasks: tool-call
   validity, and drafts passing the provenance validator (N1).
4. **Model choice is a routine, not a one-off.** A candidate is evaluated with `cmd/bench` (speed, fit)
   plus, from milestone 5, a fixed set of research tasks (quality). Changing the default is a PR with
   both results attached. This is also a task the agent can eventually do for itself
   ([design §1](../design.md#1-problem)).
5. **No KV cache quantisation for now.** The default doesn't need it, and `OLLAMA_KV_CACHE_TYPE` is
   server-wide, so it would also change every other model on the machine. It stays the first lever to
   try if a candidate wins on quality but needs more context than fits
   ([runbook §6](../target-machine.md#6-does-kv-cache-quantisation-engage)).

## Update, 2026-10-06: the first quality numbers

Milestone 5 left behind `cmd/evaltools`: eight questions whose right first move is known, five runs
each. Every model made 40 well-formed calls out of 40 to a real tool, declined to call one for the two
questions that needed none, and chose the right window every time but one: asked about "the next six
months", all three often requested 180 days though the tool covers at most 120. Applying that limit
strictly, the 9B scored 37/40 and 36/40 over two runs, the 35B MoE 34/40 and 32/40, the 27B 38/40
and 37/40 ([benchmarks](../benchmarks/README.md)).

At five runs per case those differences are noise; nothing here overturns the decision. The useful
findings are about the agent rather than the models: a limit stated in the tool's description or
schema is ignored about as often either way, so the agent enforces it in code and tells the model,
which then retries correctly. The answer can still overstate what the data covers, which is what the
provenance validator (milestone 6) is for.

## Consequences

- Milestone 8's budgets (design §3.2) start from the 9B's numbers: about 4,400–5,000 prompt tokens and
  about 80 generated tokens per second, up to 64K context.
- Milestone 5 needs an evaluation set that can be rerun against any model name, not only the default.
- A model with no `thinking` capability (checked with `qwen3-coder:30b`) accepts `think:false`, so
  trying models from other families needs no code change.
