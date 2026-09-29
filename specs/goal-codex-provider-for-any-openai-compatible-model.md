---
title: Goal — A node runs any OpenAI-compatible model: a Codex provider serves Kimi behind vLLM
kind: plan
status: running
parent: prd
summary: node.yaml's codex block takes a `providers` map so an account can name an OpenAI-compatible endpoint (vLLM, SGLang) instead of OpenAI; the provider is declared first in the session's config.toml, the key rides OPENAI_API_KEY, and its own price table is the only one its models are costed at.
---

# Goal — A node runs any OpenAI-compatible model: a Codex provider serves Kimi behind vLLM

Mesh goal `codex-provider-for-any-openai-compatible-model`, running.

## Statement

A customer runs Kimi on two NVIDIA DGX Sparks behind vLLM and wants it
for some of their nodes. An adapter is the agent CLI; a provider is
where its tokens come from — two axes, not one, so Kimi (and Qwen,
DeepSeek, a local Llama) is a provider, not a third adapter. The node
had only the adapter axis: Claude Code is already steerable, because the
node strips `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`,
`CLAUDE_CODE_OAUTH_TOKEN` and `CLAUDE_CONFIG_DIR` from every session but
not `ANTHROPIC_BASE_URL`, so an operator who exports one points every
Claude session on that node elsewhere. Codex had no such seam: it reads
its provider from `CODEX_HOME/config.toml`, and
`internal/harness/codex.go`'s `prepareHome` wrote that file itself on
every session — the MCP servers and nothing else — in both branches,
including the one where an account brings its own home, so a
hand-written `model_providers` block was overwritten every run. vLLM and
SGLang serve OpenAI's wire, so Codex is the right host for a local
model: no protocol translation, and Codex already reports the token
counts the usage ledger wants off `turn.completed`.

The fix gives node.yaml's `codex` block a `providers` map, by name:
`base_url`, an optional `wire_api` (`chat`, the default, or
`responses`) and the list prices per million tokens of the models it
serves. An account names one with `provider:`. The provider travels on
`harness.CodexAccount`, so it follows the account a session was chosen
onto and one node may run real Codex and a local model side by side. It
is declared first in the session's `config.toml` as `model_provider` and
its `[model_providers.<name>]` table, and on its own makes the file
worth writing — a session with no MCP server still needs its endpoint.

Deliberately out of scope: choosing a model per project. Work is routed
to nodes by org, so a project cannot yet ask to run on the Kimi node;
that is recorded as a platform question rather than built here.

See [docs/adr/adr-015-a-provider-serves-any-openai-compatible-model.md](../docs/adr/adr-015-a-provider-serves-any-openai-compatible-model.md),
AGENTS.md's `**Codex**` bullet, and mesh-specs' TRD Codex adapter
section.

## Acceptance

`go vet ./...` clean, `gofmt -l .` empty, `go build ./cmd/mesh/` and the
linux/amd64 cross-build both fine, and `go test ./...` green.

Harness tests: the rendered config.toml for a provider alone is exactly
`model_provider`, the `[model_providers.<name>]` table (name, base_url,
wire_api, `env_key = "OPENAI_API_KEY"`), followed by one blank line and
the MCP tables; `responses` renders as the wire when asked; OpenAI
itself (the zero provider) renders nothing; a provider name TOML will
not take bare is quoted in the table header. Price tests: a provider's
own table is used, and a model absent from it is unpriced even when
OpenAI's table has that name.

Config tests: the goal's node.yaml example loads, the account resolves
its provider for the adapter, and `cached_input`/`cache_write` left out
are the input price. Four refusals at load, each naming its reason: a
provider with no `base_url`, a `wire_api` that is neither `chat` nor
`responses`, an account naming a provider not in `codex.providers`, and
a plan login (`home:`) naming a provider.

Docs: `docs/adr/adr-015` records the decision, its consequences and the
out-of-scope routing question; AGENTS.md's Codex bullet says how to
configure a provider and states the two rules. The mini takes the
change through the release path.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness: a CodexProvider on the account is declared first in config.toml, makes the file worth writing, and is the only price table its models are costed at | merged | mesh/codex-provider-for-any-openai-compatible-model-1 | https://github.com/runsheet/mesh-cli/pull/490 (572007a) | $1.68 |
| 2 | Config: node.yaml's codex block takes a providers map, an account names one with provider:, and load refuses the four bad shapes | review (open) | mesh/codex-provider-for-any-openai-compatible-model-2 | https://github.com/runsheet/mesh-cli/pull/491 | $1.48 |
| 3 | mesh-specs: the TRD's Codex adapter section records providers, where the key rides, and that a provider's own price table is the only one | merged | mesh/codex-provider-for-any-openai-compatible-model-3 | https://github.com/runsheet/mesh-specs/pull/114 (b7662c8) | $1.02 |
| 4 | Goal doc: codex-provider-for-any-openai-compatible-model, what landed, the release path to the mini, and the routing question left with the platform | running | mesh/codex-provider-for-any-openai-compatible-model-4 | — | — |

Item 2's pull request had not merged when this document was written;
its evaluation already passed and it is awaiting the platform's own
merge of a reviewed item.

## What landed

**Item 1 — the harness (runsheet/mesh-cli#490).** `CodexProvider` {Name,
BaseURL, WireAPI, Prices} on `harness.CodexAccount`, with `IsZero`
(empty Name or BaseURL means OpenAI itself), `TOML()` and `Price(model)`.
`codexConfig(job, provider)` declares the provider first —
`model_provider = "<name>"`, a blank line, then
`[model_providers.<name>]` with `name`, `base_url`, `wire_api` (`chat`
when empty) and `env_key = "OPENAI_API_KEY"` — before the MCP tables,
and a provider alone now makes the config non-empty, so `prepareHome`'s
`if cfg != ""` short circuit no longer skips the write. The
issued-credential home (`credentials.go`'s `PrepareCodexAccount`, an
OpenAI login with `CODEX_HOME` supplied by the job) always gets the zero
provider, never the harness's own account's, so an issued login is never
pointed at a local endpoint. `Run`'s `turn.completed` progress cost and
the parsed `Result` are both re-priced through the account's
`CodexProvider.Price`: a model absent from the provider's table is
`CostUSD` 0 with `Usage.Unpriced` true, even when `CodexPrices` (OpenAI's
own table, unchanged) has that name — `ParseCodexStream` keeps its
signature and OpenAI's prices for its own callers.

**Item 2 — node.yaml (runsheet/mesh-cli#491, in review).** `CodexConfig`
gains `Providers map[string]CodexProviderConfig`; `CodexProviderConfig`
{BaseURL, WireAPI, Prices — `CodexPriceConfig` {Input, CachedInput,
CacheWrite, Output} in dollars per million tokens} and its `Harness(name)`
that builds a `harness.CodexProvider`, defaulting `cached_input` and
`cache_write` to the input price when left out. `CodexAccountConfig`
gains `Provider string`; `Harness(providers)` resolves the named one.
**The two rules, at load:** the key rides `OPENAI_API_KEY`, which the
harness already exports for an api-key account, so a provider declares
no variable of its own and an account naming one must be an api-key
account; and a provider's price table is the only one its models are
costed at, `CodexPrices` staying OpenAI's alone. **The four refusals**
`Validate` raises, each naming its reason: a provider with no
`base_url` ("base_url is required"), a `wire_api` other than `chat` or
`responses` ("want chat or responses"), an account naming a provider not
in `codex.providers` ("is not in codex.providers"), and a plan login
(`home:`) naming a provider ("needs api_key_env or api_key_file") —
because its `auth.json` would never reach the endpoint.
`internal/commands/nodecmd.go` passes `cfg.Codex.Providers` to
`Harness`. AGENTS.md's Codex bullet documents `codex.providers`,
`base_url`, `wire_api`, `provider:` and the two rules, pointing at
adr-015.

**Item 3 — the TRD (runsheet/mesh-specs#114).** The TRD's Codex adapter
section now records the `providers` map, that the key rides
`OPENAI_API_KEY` (so a plan login naming a provider is refused), and
that a provider's own price table is the only one its models are costed
at — `CodexPrices` stays OpenAI's. One retry: the first evaluation
failed because the new ADR link left "mesh-cli" outside the linked
label, unlike the section's existing mesh-cli ADR link; the retry
matched the existing style.

**The node.yaml example (adr-015, this goal's acceptance):**

```yaml
codex:
  providers:
    kimi:
      base_url: http://spark-1.lan:8000/v1
      prices:
        kimi-k3: { input: 0.55, output: 2.20 }
  accounts:
    - name: kimi
      api_key_env: KIMI_API_KEY
      provider: kimi
  models:
    opus: kimi-k3
```

## After merge

Once items 1–2 land on `main` (item 3 already has, in mesh-specs), the
mini takes the change through the release path: a merge to `main`
touching `internal/` (and `cmd/`, `images/node/`, `go.mod` or `go.sum`)
is tagged by `release-on-merge.yml`, signed and built by `release-tag.yml`,
and the node self-updates the next time its heartbeat's desired version
moves past the one it runs — `GetDesiredVersion` fetches and verifies
the new binary, swaps it and restarts, adopting its running sessions.

The hand deploy is the fallback to the release path, per AGENTS.md:

> Deploying to the mini by hand is the fallback to the release path:
> build for linux/amd64, `scp` to `~/bin/mesh.new` and `mesh node
> upgrade --binary ~/bin/mesh.new`: the binary is swapped (`.prev` kept),
> the node restarts at once and adopts its running `claude -p` sessions.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh
scp mesh-linux mini:~/bin/mesh.new
ssh mini mesh node upgrade --binary ~/bin/mesh.new
```

No live check on the mini is recorded yet: item 2's pull request has not
merged, so `main` does not yet carry the config side of this change.

## What this project did not build

**Choosing a model per project.** Work is routed to nodes by org, not by
model, so a project cannot yet ask to run on the Kimi node — a direction
or a goal would need to carry the preference, and the scheduler would
need to honour it. adr-015's consequences record this as a platform
question rather than building it here.

**A hosted endpoint's prices are the operator's to declare.** A
provider's price table is the only one its models are costed at, so a
model absent from it records tokens and no money — right for a model the
operator runs for free, wrong for a hosted endpoint that bills by the
token. This project does not validate that a provider names a price for
every model it serves; an operator who points `provider:` at someone
else's paid endpoint without pricing it will see free usage on a bill
that is not.

## Log

- 2026-09-29 · item 1 (#490) merged: the harness's `CodexProvider`, its config.toml rendering and re-pricing, and adr-015.
- 2026-09-29 · item 3 (mesh-specs#114) merged after one retry on the ADR link's label style: the TRD's Codex adapter section.
- 2026-09-29 · item 2 (#491) evaluated PASS on both families; awaiting merge.
- 2026-09-29 · item 4 (this document) written; item 2 not yet merged, so the Items table's status and the "no live check yet" note record that.
