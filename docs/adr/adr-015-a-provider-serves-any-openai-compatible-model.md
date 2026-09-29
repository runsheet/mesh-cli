---
summary: A node.yaml `codex.providers` entry is an OpenAI-compatible endpoint — a vLLM or SGLang server in front of a model the operator runs — that a Codex account names; the adapter declares it in the session's config.toml as `model_provider` with a `base_url`, the key rides OPENAI_API_KEY as it already does, and the provider's own price table is the only one its models are costed at, so a local model records tokens and no money rather than OpenAI's prices for a name it happens to share.
status: accepted
---

# ADR-015: A provider serves any OpenAI-compatible model

2026-09-29. Extends [ADR-005](adr-005-accounts-on-the-node.md) (the
accounts a session runs on) with where the tokens come from, and the
Codex adapter's session home (`internal/harness/codex.go`).

## Context

A node runs one of two agent CLIs — `claude -p` or `codex exec` — and
until now each talked only to its vendor. Claude Code is already
steerable: the node strips `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
`CLAUDE_CODE_OAUTH_TOKEN` and `CLAUDE_CONFIG_DIR` from every session but
not `ANTHROPIC_BASE_URL`, so an operator who exports one points every
Claude session on that node at another endpoint. Codex had no such seam:
it reads its provider from `CODEX_HOME/config.toml`, and `prepareHome`
writes that file itself on every session — the MCP servers and nothing
else — in both branches, including the one where an account supplies its
own home. A hand-written `model_providers` block was overwritten each
run.

This came up as a customer wanting two DGX Sparks serving Kimi behind
vLLM for some of their nodes. vLLM and SGLang serve OpenAI's wire, so
the Codex adapter is the natural host for it: no protocol translation,
and Codex already reports the usage the ledger wants
(`turn.completed`'s `input_tokens`, `cached_input_tokens`,
`cache_write_input_tokens`, `output_tokens`).

## Decision

**An adapter is the agent CLI; a provider is where its tokens come
from.** These are two axes, not one. Kimi is not a third adapter — it is
a model behind a provider — so the change that carries it carries Qwen,
DeepSeek and a local Llama at no further cost.

**`codex.providers` in node.yaml**, a map by name:

```yaml
codex:
  providers:
    kimi:
      base_url: http://spark-1.lan:8000/v1   # vLLM serves OpenAI's API under /v1
      wire_api: chat                         # or responses; empty is chat
      prices:                                # USD per million tokens, optional
        kimi-k3: { input: 0.55, output: 2.20 }
  accounts:
    - name: kimi
      api_key_env: KIMI_API_KEY
      provider: kimi
  models:
    opus: kimi-k3
```

An account names one, or none and runs on OpenAI. `CodexProvider`
travels on `harness.CodexAccount`, so the provider follows the account a
session was chosen onto and one node may run real Codex and a local
model side by side.

**The provider is declared first in the session's `config.toml`**, and
on its own makes the file worth writing — a session with no MCP server
still needs its endpoint, which is why `codexConfig` now takes the
provider and the empty-config short circuit no longer skips it.

**The key rides `OPENAI_API_KEY`.** The harness already exports it for
an api-key account, so `env_key` is always that and a provider declares
no variable of its own. A server that checks no key still wants one
sent: hence a provider account must be an api-key account, and a plan
login with a provider is refused — its `auth.json` would never reach the
endpoint. Give a keyless server any non-empty value.

**A provider's price table is the only one its models are costed at.**
`CodexPrices` stays OpenAI's alone. A model with no price recorded costs
nothing, which is right for one the operator runs and wrong for a hosted
endpoint — so name the price when there is a bill. Without this rule a
local model called `gpt-6-sol` would be billed at OpenAI's rate for the
name.

## Consequences

- The usage ledger keeps working: tokens, wall time, adapter and model
  all record, and only `list_cost_cents` is zero for an unpriced model.
  Nothing in the platform changes — `adapter` and `model` are already
  free strings on `UsageReport` — so this needs no platform deploy
  first, unlike a change to what the node claims or reports.
- A node's provider is a property of its accounts, and work is routed to
  nodes by org, not by model. A project that must run on a particular
  model cannot say so yet; that is a platform question (a direction or a
  goal would carry the preference, and the scheduler would honour it),
  and it is deliberately not answered here.
- Budgets still do not bind a Codex session: the CLI takes no budget
  flag, so `Job.MaxBudgetUSD` is the caller's to hold, as ADR-005 says.
  An unpriced model makes that gap wider, since the spend the platform
  sees is zero — the `Timeout` is the real cap.
- `wire_api` is validated against `chat` and `responses` at load, so a
  typo fails `mesh node start` rather than every session.
