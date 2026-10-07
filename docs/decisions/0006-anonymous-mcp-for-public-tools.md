# 0006 — Connect to Quantic's MCP server anonymously; a service token when it's needed

**Status:** accepted · **Date:** 2026-10-06 · **Revisit at:** the first task that needs an
authenticated reference tool, or the first run that hits the anonymous rate limit

## Context

Design [open question 1](../design.md#5-open-questions) asked how a headless daemon authenticates to
Quantic's MCP server, since OAuth assumes a person clicking "Allow". The options were a long-lived
service token scoped to read-only tools, a direct internal API, or a stored OAuth refresh token.

The preferred answer going in was the service token, limited to the public reference tools. The
point of it is design N4: an agent with no user identity can't read anyone's portfolio, whatever it is
told or prompted to do.

## What the server already does

Reading `QuanticWeb.Plugs.McpAuth` and `QuanticWeb.MCP.Server` (quantic `main`, 2026-09-09):

- **No `Authorization` header → anonymous access**, rate limited to 60 requests a minute per IP. The
  public reference tools answer anonymous callers: `dividend_calendar`, `get_stock`, `search_stocks`,
  `screen_stocks`, `list_stocks`. They read the database only: no provider calls, no writes.
- **Portfolio tools refuse anonymous callers** with a tool error (`isError: true`) explaining how to
  authenticate. Captured: `internal/mcp/testdata/call-private-anonymous.json`.
- **Two credentials exist, both tied to a user**: an OAuth access token from the "Log in with Quantic"
  flow, and a personal access token (`qtc_…`, hashed at rest). Either one gives the caller that
  user's portfolio tools.
- A wrong token is a `401`, never a silent fall back to anonymous.
- `compare_stocks` and `get_stock_research` are reference data but require *some* authenticated
  caller.

So the service token's main property, no user identity and therefore no portfolio access, is
already what an anonymous caller gets. And it's enforced by the server, not by the agent.

## Decision

1. **The agent connects anonymously.** Milestone 5's tool and the Week Ahead's data
   (`dividend_calendar`, `get_stock`) are all public.
2. **The agent must never be given a personal access token or OAuth grant.** Those act as a user and
   unlock that user's portfolio, which N4 forbids. The client accepts a token (from
   `QUANTIC_MCP_TOKEN`, environment only) so a future service token can be supplied without code
   changes.
3. **The service token stays the plan for when an authenticated reference tool is needed**
   (`compare_stocks`, `get_stock_research`). That is a server change in the private repo: a token
   type that resolves to a scope with no user, which reference tools accept and portfolio tools refuse
   exactly as they refuse anonymous callers today.

## Consequences

- **The rate limit becomes a budget.** 60 requests a minute per IP, shared with anything else on the
  same address. The Week Ahead needs the calendar plus one `get_stock` per company, often 20–40 calls.
  The research loop's call budget (design §3.2) must stay under it, or pace its calls. Hitting the
  limit is a `429`; the client treats it as a failure today, and milestone 8 should retry it after a
  pause. *(Done in milestone 8: the server counts in fixed one-minute windows and sends no
  `Retry-After`, so the client backs off exponentially, with jitter, for at least 60.5s in all before
  reporting `mcp.ErrRateLimited`.)*
- **The calendar has no amounts or yields.** `dividend_calendar` returns name, symbol, sector,
  ex-date and frequency. The Week Ahead table's amount and yield columns
  ([content plan](../content.md)) come from `get_stock`, one call per company.
- **Tool descriptions are the agent's own.** The server's description of `dividend_calendar` ends with
  buying advice ("buy before the ex-date to receive the next dividend"); the agent's output is
  informational only (N5), and what a model is told a tool is for shapes what it writes. A test checks
  that our argument schema still matches the one the server publishes.
- **Nothing to store, rotate or leak.** The daemon has no Quantic secret today.
