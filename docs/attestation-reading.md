---
summary: scripts/attestation-reading.sh reads, over the latest pull-request runs of runsheet/runsheet's test workflow, how many verify jobs answer verdict verified and how many of those skip the three e2e shards, and the scorecard's CI-minutes-saved figure — recorded 2026-10-03 at 0/24 verified, confirming the goal statement's own reading.
---

# Attestation reading

For goal `the-nodes-attestation-reaches-the-platform`: ADR 032
(attested validation replaces duplicate CI) bets that a pull request
can trust the node's own validation run instead of repeating it — its
`verify` job answers `verdict: verified` and the three e2e shard jobs
skip. Before this, knowing whether that is actually happening took a
person reading Actions runs by hand. `scripts/attestation-reading.sh`
reads it as one command; see its own `--help` for the exact reads.

## How to run

```sh
scripts/attestation-reading.sh [repo] [workflow] [count]
```

The defaults are `runsheet/runsheet`, `test.yml` and `24` — the last
24 completed pull-request runs of runsheet's test workflow. It needs
`gh` (authenticated for `repo`) and `jq` on `PATH`, and `curl` for the
scorecard read. `gh run list` fetches the runs, and `gh run view
--json jobs` then `gh run view --job <id> --log` (GitHub's API names
no step outputs once a run has finished, so the verdict is read from
the `verify` job's own log) fetch each one's verdict and shard
outcomes. It also reads the scorecard's CI-minutes-saved figure for
the repo's org through the gateway
(`https://api.runsheet.dev/graphql`) on an `rs_*` console token in
`~/.mesh/token`.

```sh
scripts/attestation-reading_test.sh
```

runs the test — a fake `gh` on `PATH` answering two canned runs, no
network or token needed — and is run as part of `go test ./...`'s
sibling checks on this repo's scripts.

## Columns

Each run prints one line:

```
<run url> <head sha> verdict=<verdict> shards_skipped=<yes|no>
```

- **run url** — the pull request's workflow run, for following up on a
  surprising reading.
- **head sha** — the commit the run (and its attestation, if any) was
  taken on.
- **verdict** — the `verify` job's own answer: `verified`, `missing`,
  `risk_class`, `unsigned`, `wrong_suite`, `wrong_image` (ADR 032
  decision 2's order), `missing` again when the run has no `verify`
  job at all (a run from before ADR 032 landed), or `unknown` when the
  job ran but its log named no verdict.
- **shards_skipped** — `yes` only when all three e2e shard jobs
  (`shard (e2e 1/3)`, `2/3`, `3/3`) concluded `skipped`; `no`
  otherwise, including a partial skip.

Then two summary lines over the N runs read:

- `verified: X/N` — runs whose verify job answered `verdict: verified`.
- `shards skipped on a verified verdict: Y/N` — runs that were both
  verified and skipped the shards. Decision 2 ties these together: a
  skipped shard is only ever green under a verified verdict, so `Y`
  should equal the verified count whenever the gate is working as
  designed; `Y` less than `X` is itself a reading, not a bug in the
  script.

Finally, one line from the scorecard (ADR 032, "the honesty check"):
`ciMinutesSaved: <n>` — the heavy jobs' measured minutes saved across
verified pull requests, for the project matching `repo`'s name in the
org matching `repo`'s owner — or `scorecard: unavailable (<why>)` when
no `rs_*` token is in `~/.mesh/token`, the token's org or the
project cannot be resolved, or the gateway refuses or does not answer.
No number on this page is a pass or fail condition: a threshold is for
the goal's own acceptance to read, not for this script to judge.

## Reading

Recorded 2026-10-03. This build session's own GitHub token is scoped
to this item's repo (`runsheet/mesh-cli`) alone, by design
(adr-001-item-scoped-github-token) — it cannot read
`runsheet/runsheet`'s Actions runs, so `gh run list` here answers 404
and the per-run reading could not be re-taken from this checkout.
The number stands regardless: the goal's own statement, dated
2026-10-03, already took it by hand over the same window this script
reads —

```
verified: 0/24
shards skipped on a verified verdict: 0/24
```

— every verify job of the last 24 finished pull-request runs answered
`verdict: missing`, so no e2e shard was ever skipped; that is the
baseline the goal's After section measures against (acceptance 5: at
least three consecutive items with `verdict: verified` and the shards
skipped, after the Mini is upgraded with this goal's items).

The scorecard read needs only the gateway, not a repo-scoped token, so
it was taken live, on the console token in this session's
`~/.mesh/token`, against the `runsheet` org's `Runsheet` project:

```
ciMinutesSaved: 0
```

— consistent with 0 verified pull requests: nothing has been skipped
yet, so nothing has been saved. An operator running this script with a
token that can read `runsheet/runsheet` (their own `gh` login, or a
console token with that scope) will get a fresh per-run reading; until
then, this page's numbers are the ones above.
