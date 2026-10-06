# Go lessons

Notes written while building `quantic-agent`, by someone whose daily languages are Elixir and Ruby.

Each lesson pairs with a milestone from the [roadmap](../../README.md#roadmap). They're not a Go
tutorial — plenty of those exist. They're the specific things that surprised me, the assumptions I
carried over from other languages that turned out to be wrong, and the reasoning behind Go's choices
where I found it non-obvious.

| # | Lesson | Milestone | Read online | Code walkthrough |
|---|---|---|---|---|
| [00](00-project-layout-and-modules.md) | Project layout, modules, and the `internal` rule | 0 — repo & design | [Modules and the `internal` rule](https://claude.ai/code/artifact/bd329fad-14ba-44b7-8188-bd98cf419424) | — |
| [01](01-a-binary-a-package-and-go-test.md) | A binary, a package, and `go test` | 1 — first code | [A binary, a package, and `go test`](https://claude.ai/artifact/PuuxbeGuttxWhboXDxbZMi) | — |
| [02](02-structs-tags-and-one-http-call.md) | Structs, tags, and one HTTP call | 2 — the model client | [Structs, tags, and one HTTP call](https://claude.ai/artifact/4QGaudzoGjNAPto7ZcxgpD) | [The Ollama client, line by line](https://claude.ai/artifact/F87xnvzrSKtwqnwsNS6ipT) |
| [03](03-errors-you-can-ask-questions-of.md) | Errors you can ask questions of | 3 — errors across the LLM boundary | [Errors you can ask questions of](https://claude.ai/artifact/CCH115UXhRfsN5N16eXRHf) | [Errors, line by line](https://claude.ai/artifact/6HdVkT2Z8FGZoyxMnyaJ5i) |
| [04](04-deadlines-and-letting-go.md) | Deadlines, and letting go cleanly | 4 — timeouts and cancellation | [Deadlines, and letting go cleanly](https://claude.ai/artifact/LCHhkZQtTehnjSNXRyVWJv) | [Deadlines, line by line](https://claude.ai/artifact/LvtwYVQmph38YeYdrjSRte) |
| [05](05-the-first-real-tool.md) | The first real tool | 5 — `dividend_calendar` end to end | [The first real tool](https://claude.ai/artifact/9Fe5FJdzbCf2jTGMyTdLNd) | [The first tool, line by line](https://claude.ai/artifact/UJFF5Uiz7EwZJ75StoJwHE) |
| [06](06-every-figure-has-a-source.md) | Every figure has a source | 6 — the provenance validator | [Every figure has a source](https://claude.ai/artifact/VneRrzy5zK36bKXuX48uzb) | [Provenance, line by line](https://claude.ai/artifact/PJ7YDwFzobrQMCoKEB5UJ7) |
| [07](07-a-memory-that-can-be-audited.md) | A memory that can be audited | 7 — SQLite: runs, drafts, audit log | [A memory that can be audited](https://claude.ai/artifact/5KnF36mtBBgY3uXfiQ4q27) | [The run history, line by line](https://claude.ai/artifact/AAZi4mJ3GCd6zz6X6LYVM7) |
| [10](10-vector-search-without-a-vector-db.md) | Vector search without a vector database | 10 — retrieval | [Vector search without a vector database](https://claude.ai/code/artifact/4cf71989-6588-4864-b3bc-314781a156b1) | — |

Lessons tell the story of a milestone: what surprised me and why. Walkthroughs go through the code
itself in reading order, the way a tech lead would with a new teammate. Walkthroughs start at
milestone 2.

Lesson 10 is out of order deliberately — it was written early because the question came up, not
because the milestone moved.

More land as the milestones do.
