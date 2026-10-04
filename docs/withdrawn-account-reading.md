---
summary: The first live reading of scripts/withdrawn-account-reading.sh against the Mini, taken before its upgrade.
---

# withdrawn-account-reading: the Mini, before the upgrade

Command:

```
scripts/withdrawn-account-reading.sh --node mini
```

Taken: 2026-10-04T22:40:30Z

Mini's version at the time (from its nodeIndexs row, org `projectmesh-io`,
`46873dfe-4c8a-59a4-949f-c82aa6065962`, status `offline`): `v0.2.286` —
before this goal's items 1-3 (the withdrawn-account fix itself) have
been deployed to it.

Output as printed:

```
== reported
Temp - Subsciprtion	holding=unknown	limits=-
Temp2 - Subscription	holding=unknown	limits=-
claude - shhemail	holding=unknown	limits=-
== platform
(none)
== stale
Temp - Subsciprtion	reason=unknown
Temp2 - Subscription	reason=unknown
claude - shhemail	reason=unknown
stale: 3
```

Honest notes on this reading:

- `limits` is not yet served as a field on `NodeIndex` by the platform
  (a live introspection query confirms it; the script falls back to
  asking for `holdingAccounts` alone when the platform refuses it, per
  its own usage text). So `limits=-` for every label here is the
  fallback, not evidence that nothing is held on that surface.
- The `platform` section reads `(none)`: `credentialIndexs` named no
  row for org `46873dfe-4c8a-59a4-949f-c82aa6065962` or its enterprise
  (it has none) at read time. Every reported label therefore reads
  `reason=unknown` rather than the specific `deleted`/`disabled`/`not
  allowed` the goal's statement describes for these same three labels
  on 2026-10-03 (and the fourth, `mesh - max20`, is not reported here
  at all today). That gap — a label's true platform status reading as
  "unknown" because this org's `credentialIndexs` is empty from this
  token's vantage point, rather than because the label is genuinely
  gone — is a real limitation of this reading as taken today, not a
  claim that the Mini's holding report is accurate.
- The node itself was offline at read time (`status: offline`), so
  this is a last-known report, not a live one.

This is **not** the goal's acceptance (4) reading. Acceptance (4) asks
for `nodeIndexs.holdingAccounts` on the Mini's rows to name only active
credentials allowed on the Mini within one heartbeat of a deletion,
with `Temp - Subsciprtion`, `claude - shhemail` and `mesh - max20`
absent, **after the Mini is upgraded** to a build carrying items 1-3.
Rerunning this exact command (`scripts/withdrawn-account-reading.sh
--node mini`) once the Mini is upgraded and back online is that
reading; it has not been taken yet.
