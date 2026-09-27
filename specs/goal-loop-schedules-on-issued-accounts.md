---
title: Goal — The node paces, claims and researches on the accounts the platform issues, not on the operator's own login
kind: plan
status: draft
parent: prd
summary: The loop schedules on the union of node.yaml's accounts and the platform-issued ones it knows, so research and normal work run on fresh credentials while pacing reflects each issued account's actual usage.
---

# Goal — The node paces, claims and researches on the accounts the platform issues, not on the operator's own login

Mesh goal `loop-schedules-on-issued-accounts`, running.

## Statement

Found 2026-09-27 on the mini (runsheet docs/HANDOFF.md, '2026-09-27: every session ran on the mini's own account'). Since mesh-cli #208, sessions run on platform-issued credentials (itemCredentials/planCredentials/researchCredentials, adr-005), but the loop's scheduling still uses node.yaml's accounts, or with none the implicit `default` (internal/node/loop.go accounts(): nodeAccounts() else DefaultAccount judged by strictest(readings)): pacing() → nodePace, accountFor()/accountToFinish() (ChooseAccount), researchDue's `accountFor(ctx, "low", false)`, the 'no account has headroom' line and the scaling/headroom loop all judge `default`. The mini's `default` reading is its exhausted seven-day window (paced until 2026-09-28T18:00Z), so research never starts ('no research'), normal-priority items sit ready ('no account has headroom for 3 item(s)') while two fresh Max accounts (mesh - max20, claude - shhemail) run every session that does start.

Make the accounts the loop schedules on the union of node.yaml's accounts and the platform-issued ones it knows: Credentials remembers every credential the claim-time read hands it (label, kind, id; never the secret) as an AccountConfig and exposes them; Loop.accounts() includes them; readings are already kept per label (limits.Observe(account, …)) so pacing per issued account works unchanged; `default` is judged only when no issued account is known and no node.yaml account exists. Persist the known labels in ~/.mesh (beside ledger.json) so a restart still knows them before its first claim, and drop one that the platform has not issued for 24h. researchDue then finds headroom on an unpaced issued subscription and claims the direction; the pacing log line names the account that paces. Keep the accounts-fallback behaviour for a platform that issues none. Amend mesh-specs trd §3 Accounts (The claim-time read / Capacity across accounts) and adr-005.

## Acceptance

mesh-cli tests: with no node.yaml accounts and the platform issuing two subscriptions, the loop's pacing is judged on those two (an exhausted reading under `default` from an earlier session does not pace the node), researchDue claims a direction on an issued subscription with headroom, accountFor chooses among issued accounts by their readings, and the 'no account has headroom' line names the accounts judged; a restart reloads the known labels; a platform that issues none leaves today's behaviour. go build/vet/test green on the mini's runners; the goal doc says the mini needs the by-hand binary upgrade after merge.

## Items

| Seq | Title | Status | Branch | PR | Cost |
| --- | --- | --- | --- | --- | --- |
| 1 | Credentials remembers every issued account, secret-free, and keeps the labels in ~/.mesh/accounts.json for 24h | merged | mesh/loop-schedules-on-issued-accounts-1 | https://github.com/runsheet/mesh-cli/pull/224 | $1.20 |
| 2 | The loop paces, claims, researches and scales on the union of node.yaml's accounts and the issued accounts it knows | open | mesh/loop-schedules-on-issued-accounts-2 | https://github.com/runsheet/mesh-cli/pull/225 | $2.40 |
| 3 | mesh-specs: adr-005 amendments — the loop schedules on the union of node.yaml's and the known issued accounts | merged | main | https://github.com/runsheet/mesh-specs/pull/46 | $0.36 |
| 4 | Goal doc: loop-schedules-on-issued-accounts, with the mini's by-hand binary upgrade after merge | — | mesh/loop-schedules-on-issued-accounts-4 | — | $0.00 |

## Upgrade

After this goal merges, mesh-cli has no deploy and the mini's binary must be upgraded by hand. Build for linux/amd64 (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o mesh-linux ./cmd/mesh`), `scp` to `~/bin/mesh.new`, and `mesh node upgrade --binary ~/bin/mesh.new` (see README.md's "Upgrading a node without ending sessions", line ~523). The first tick after the upgrade knows no issued labels until its first claim unless `~/.mesh/accounts.json` already exists from a prior session; the session's credentials are handed fresh by the platform on every claim regardless, but the loop's pacing reads from the stored labels so the first research claim won't find issued accounts until the second tick.

The upgrade marks the moment the loop's pacing reads the union of node.yaml's and issued accounts: before the upgrade, pacing is judged by node.yaml and `default` only; after, every issued account's reading counts toward headroom and pace decisions, so research and normal work can run on accounts the platform issues, not the operator's login (the `default`).

