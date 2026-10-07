# Benchmarks

Raw `cmd/bench -json` output from the target machine, kept as the evidence behind model decisions.
Read with [decision 0005](../decisions/0005-default-model-by-measurement.md); how to run a new one is
in the [runbook](../target-machine.md#5-run-the-benchmark).

| File | What | Conditions |
|---|---|---|
| [`2026-09-24-bench.json`](2026-09-24-bench.json) | The default three models at 8K, 32K, 64K | f16 KV cache |
| [`2026-09-26-moe-vs-dense.json`](2026-09-26-moe-vs-dense.json) | `qwen3.6:35b` (MoE) vs `qwen3.6:27b` (dense) at 8K, 32K | f16 KV cache |
| [`2026-10-06-toolcalls-description-only.json`](2026-10-06-toolcalls-description-only.json) | `cmd/evaltools`: tool choice and arguments, 8 cases × 5 runs, three models | The 120-day limit stated only in the tool description |
| [`2026-10-06-toolcalls-schema-maximum.json`](2026-10-06-toolcalls-schema-maximum.json) | The same evaluation, rerun | The limit also in the schema (`maximum: 120`) and enforced by the agent |
| [`2026-10-07-writer/`](2026-10-07-writer/README.md) | `agent -research` end to end: milestone 7's single loop against three versions of milestone 8's writing phase, 9 runs each | `qwen3.5:9b`, live quantic.finance data |

Common to both: RTX 4070 Ti Super 16GB (driver 610.57.04), Ryzen 7 7800X3D, 64GB RAM, Ollama 0.34.4,
flash attention on, one parallel slot. The desktop was in normal use, holding about 1.2GB of VRAM
(Hyprland, a browser, Steam) — the condition the agent actually runs in.

`generated_tokens` below `generated_tokens_requested` means the model stopped early and that row's
generation rate is a small sample. The first row of a run also includes a cold start; treat
`qwen3.5:9b` at 8K in the first file as unreliable for generation speed.

**Reading the tool-call files.** Each outcome is one run of one case: whether the model's *first* reply
was the right move (`correct`), whether its call was well-formed (`valid`), and what it called with
which arguments. The second run judges strictly: a request over 120 days is invalid. Applying that
rule to the first run too, the totals compare like this:

| Model | Run 1 | Run 2 | Requests over 120 days, run 1 / run 2 |
|---|---|---|---|
| qwen3.5:9b | 37/40 | 36/40 | 3 / 3 |
| qwen3.6:35b | 34/40 | 32/40 | 6 / 8 |
| Qwen3.8-27B `UD-IQ3_S` | 38/40 | 37/40 | 2 / 3 |

Putting the limit in the schema didn't change how often models exceed it; five runs per case can't
separate these models either. See [decision 0005](../decisions/0005-default-model-by-measurement.md).
