# 0007 — Continue the agent in Python; this Go version is finished

**Status:** accepted · **Date:** 2026-10-07

## Context

The agent was written in Go first of all because I wanted to learn Go (README, "Why Go"). That reason
now has a better home: [quantic-cli](https://github.com/fleveque/quantic-cli), a command-line client
for Quantic, is a kind of program Go is the usual choice for.

Without it, the question is which language suits the agent's own goals best: a local agent that
drafts data-grounded content for Quantic, does varied tasks including evaluating new models, and is a
place to experiment with local agents, local models and retrieval (RAG). Go, Elixir and Python were
compared.

## The comparison

- **Python** has the ecosystem the next milestones need: document parsing and chunking, rerankers
  and retrieval evaluation (milestone 10), running models in-process beyond Ollama, model evaluation
  and Hugging Face Hub access (evaluating new models), notebooks for analysing runs. An experimentation
  project is limited by how fast the next idea can be tried and measured, and almost every next idea
  is Python-first. Its weaknesses for a long-running daemon (packaging, optional typing) are covered
  by `uv`, Pydantic and a strict type checker in CI.
- **Elixir** has the best runtime for the daemon parts (OTP supervision, a GenServer for the GPU
  queue) and is the author's daily language. Its AI ecosystem lags: new model architectures reach
  Bumblebee late, and retrieval and evaluation tooling is thin. It would win if the goal became a
  production feature inside Quantic; the agent would still be a separate app, since it must run where
  the GPU is.
- **Go** is good at the plumbing, and works today, but has the least of what retrieval, document
  handling and model evaluation need.

## Decision

1. **The agent continues in Python**, in a new repository that takes the name
   [quantic-agent](https://github.com/fleveque/quantic-agent), with the same goals, design,
   non-negotiables and way of working: one PR per milestone, a lesson and a line-by-line walkthrough
   for each, now comparing Python with Go.
2. **Milestones 0–8 are ported in order**, one PR each, before milestone 9: the cheapest moment to
   switch, since milestones 10 and 11 are where Python pays off most.
3. **This repository is renamed `quantic-agent-go`, marked deprecated, and archived.** Its lessons
   and walkthroughs stay as the record of learning Go.
4. **Toolchain:** `uv`, the latest stable Python, `ruff`, `pyright` (strict), `pytest`, `httpx`,
   Pydantic.

## Consequences

- What is language-independent carries over as it is: the design, ADRs 0001–0006, the benchmarks,
  the prompts, the captured Ollama and MCP fixtures, and the provenance test cases.
- Links to `github.com/fleveque/quantic-agent` written before the rename now reach the Python
  repository. Those in this repository, the published lesson pages and quantic-cli are updated to
  `quantic-agent-go`.
