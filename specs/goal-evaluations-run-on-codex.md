---
title: Goal — Evaluations run on Codex (GPT-6) from a ChatGPT Pro account the console holds, with Claude as the fallback — and the scorecard compares the families
kind: plan
status: draft
parent: prd
summary: The evaluator chooses Codex when a Codex account has headroom for the item's priority and falls back to Claude when not, usage reports name both adapters so the scorecard shows first-pass and false-pass by model family, and the console holds a ChatGPT Pro credential.
---

# Goal — Evaluations run on Codex (GPT-6) from a ChatGPT Pro account the console holds, with Claude as the fallback — and the scorecard compares the families

Mesh goal `evaluations-run-on-codex`, running.

## Statement

Chris is buying ChatGPT Pro (2026-09-28) so Codex — GPT-6 Astra/Sol/Luna — carries Mesh's evaluations: 499 evaluation sessions and $484 (16% of spend) last week ran on Claude. The `codex-adapter` goal already made Codex the evaluator's second opinion on urgent and high goals (`trd` §1 WorkItem, §3 The Codex adapter; mesh-cli `adr-005-codex-second-opinion`). Today a Codex session can only run on the operator's own login — the platform's credentials (vendor openai, kind oauth_token or api_key) are handed to sessions by the claim-time reads, but the node maps only vendor anthropic rows to accounts and the Codex harness has no path from an issued credential to a session's CODEX_HOME.

Make the evaluator's family choice dynamic: (1) **Node**: an issued credential with vendor openai and serves containing 'codex' is a Codex account — kind oauth_token carries the ChatGPT login (the auth.json `codex login` writes, sealed as the secret; the node writes it into the session's temporary CODEX_HOME/auth.json and deletes it with the home), kind api_key carries an OpenAI key (CODEX_API_KEY/OPENAI_API_KEY in the session env, caps enforced by the platform as for Anthropic keys). Its rate-limit standing is read from Codex's output. (2) **Evaluator**: the evaluator prefers Codex when a Codex account has headroom for the item's priority and falls back to Claude otherwise — the choice is per session, recorded in the usage report (adapter: codex, model: gpt-6-…) so the ledger and the scorecard's by-model rows show both families. The planner may pin an item's evaluator to a family through the existing agent definitions. (3) **Console**: adding an openai credential explains what to paste (the auth.json from `codex login` on any machine, or an API key) and which roles use it. (4) **Scorecard**: first-pass rate and evaluator false-pass rate by model family, so 'does GPT-6 evaluate as well as opus' is a number.

Runsheet-side pieces (credential kind wording, scorecard) are small; runsheet's items carry them. Read runsheet specs/adr-019-credentials.md and mesh-specs adr-005, adr-006 first; amend both. The mini needs the by-hand node upgrade after merge.

## Acceptance

mesh-cli tests: an issued openai/oauth_token credential lands as the session's CODEX_HOME/auth.json and is gone when the session ends; an openai/api_key lands as CODEX_API_KEY; the evaluator chooses Codex when it has headroom and Claude when it does not; usage reports name adapter codex and the model. go test ./... passes. Runsheet e2e: an openai credential is served to the evaluator's claim-time read; the scorecard shows first-pass and false-pass by family. Live: Chris adds the Pro account as a credential in the console, the mini (after upgrade) evaluates the next item on GPT-6, the item page shows it, and the Nodes page shows the OpenAI account's windows beside the Claude ones.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Harness: Codex runs in a CODEX_HOME the session hands it, and reads the plan's rate limits from the session file | merged | mesh/evaluations-run-on-codex-1 | https://github.com/runsheet/mesh-cli/pull/241 | $1.45 |
| 2 | Node: an issued openai credential serving codex is a Codex account — the login lands in the session's CODEX_HOME/auth.json, a key in CODEX_API_KEY, both gone with the session | merged | mesh/evaluations-run-on-codex-2 | https://github.com/runsheet/mesh-cli/pull/242 | $2.30 |
| 3 | Node: the evaluator prefers Codex when a Codex account has headroom for the item's priority, falls back to Claude, and reports adapter codex with the model | merged | mesh/evaluations-run-on-codex-3 | https://github.com/runsheet/mesh-cli/pull/250 | $2.15 |
| 4 | mesh-specs: TRD and ADR 005/006 — an openai credential is a Codex account, the evaluator runs on the family with headroom, and the scorecard compares families | merged | mesh/evaluations-run-on-codex-4 | https://github.com/runsheet/mesh-specs/pull/53 | $0.85 |
| 5 | Goal doc: evaluations-run-on-codex, the mini's by-hand node upgrade, node.yaml notes, and the runsheet-side pieces still owed | — | mesh/evaluations-run-on-codex-5 | — | $0.00 |

## After merge: the mini

After this goal merges, mesh-cli has no deploy and the mini's binary must be upgraded by hand. Build for linux/amd64 (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh`), `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions", line ~523). The first evaluation after the upgrade runs on whichever family has headroom; no Codex credential means Codex is never offered (the `ChooseAccount` step before an evaluation pre-filters by headroom and the account must exist), so the evaluator falls back to Claude as today.

## How to add the ChatGPT Pro account

1. On any machine with `codex` installed, run `codex login`. It prompts for username and password, stores credentials in `~/.codex/auth.json` (mode 0600).
2. Read the file: `cat ~/.codex/auth.json`. Copy the entire JSON.
3. Go to the runsheet console's **Settings → Credentials**. Click **Add credential**.
4. Fill the form:
   - **Vendor:** OpenAI
   - **Kind:** OAuth Token (for a ChatGPT Pro login) or API Key (for an OpenAI API key)
   - **Secret:** Paste the entire `~/.codex/auth.json` (for OAuth Token) or the API key (for API Key)
   - **Serves:** `codex`
   - **Project:** (the project the node will use the credential on)
5. Save. The node's claim-time read (`gateway.Session`, `itemCredentials`) will hand the credential to evaluation sessions when a Codex account has headroom.

**Why:** The node maps issued credentials to Codex accounts by `serves` — a credential with `serves` containing 'codex' and vendor 'openai' becomes a Codex account, exactly as vendor 'anthropic' credentials become Claude accounts. kind oauth_token's secret goes into the session's `CODEX_HOME/auth.json` (0600, removed when the session ends). kind api_key's secret goes into the session's env (CODEX_API_KEY and OPENAI_API_KEY), caps enforced by the platform. The rate limit is read from the Codex session's own output (or defaults) and paced per account like a Claude subscription.

## Owed on the runsheet project

This goal could not carry the runsheet-side pieces because they live outside runsheet/mesh's repos:

- **runsheet/console:** Credential form wording for openai vendor. The form's `Kind` dropdown and `Secret` label should explain what to paste for oauth_token (the auth.json from `codex login`) and api_key (an OpenAI API key). The form's `Serves` field should name `codex` in a note. A credential with vendor openai and `serves: codex` is served to evaluation sessions.
- **runsheet/gateway:** scorecard.go reads the usage ledger's `adapter` and `model` columns and computes first-pass rate and evaluator false-pass rate per model family. The `usage` record on `EvaluateWorkItem` (`gateway/mutations.go`) carries `adapter: "codex" | "claude-code"` and the model it ran on. The integration is per-model-family, not per-model — `gpt-6-astra`, `gpt-6-sol` and `gpt-6-luna` roll up to family `codex`, and `claude-opus-…`, `claude-sonnet-…`, `claude-haiku-…` roll up to family `claude`.
- **runsheet/mesh-portal:** Nodes page shows the OpenAI account's rate-limit windows beside the Claude ones. The node's heartbeat carries the usage report `limits` per account (subscription windows or API key budget windows; `adapter` field names the vendor). A Codex account sends `adapter: codex` and windows under the same schema as Claude's `adapter: claude`.

## Design

This goal amends mesh-specs' `trd` §1 WorkItem (second-opinion paragraph) and §3 (Which model a session runs on, The Codex adapter, Accounts), and adds mesh-cli ADR-011 `evaluations-run-on-the-family-with-headroom`. See item 4's PR and the `.mesh/context/` copies of both.

The evaluator's family choice: before an evaluation starts, the node judges headroom across all Codex accounts (by Codex-specific limits read from Codex's output) and all Claude accounts (by their subscription windows or API key budgets). It prefers **Codex when a Codex account has headroom** for the item's priority, and runs on **Claude otherwise**. A planner may pin an item's evaluator to a family by naming an agent model starting `gpt-` (Codex pin) or `claude-` / bare tier (Claude pin); a pinned family with no headroom waits (not claimed, not failed), never falling back.

An item of an urgent or high goal whose first evaluation passes is evaluated again on the other family with the same prompt and workspace (trd §1 WorkItem); both passes is passed, a second fail is failed, notes carry both verdicts labelled by family and adapter, and the usage report's `adapter` and `model` name the first family. A second opinion that errors, times out, reaches no verdict, or has no account with headroom keeps the first verdict with a note.

## Node.yaml: the codex: block

The `codex:` block in node.yaml is now **optional**. It gives the node its own Codex account (before `--prepare-account` time, outside the claim-time read); an issued openai credential is a Codex account too (the node discovers it via claim-time reads). With neither, evaluations run on Claude (the second opinion, if any, also runs on Claude, making it redundant — but harmless — on a node without Codex headroom).

**Default model mappings** (unchanged from item 2's Codex work, but worth restating):
- `gpt-6-astra` (the live model as of 2026-09-28)
- `gpt-6-sol` and `gpt-6-luna` (the astra replacements)
- `gpt-6` (alias to `gpt-6-astra`, the default when a planner names bare `gpt-6`)

Analogous to Claude's `claude-opus-5-5` / `claude-sonnet-5` / `claude-haiku-4-5-20251001` and bare-tier names `opus`, `sonnet`, `haiku`.
