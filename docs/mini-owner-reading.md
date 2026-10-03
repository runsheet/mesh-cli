---
summary: scripts/mini-owner-reading.sh reads, after the Mini is upgraded and re-registered onto the owner contract, whether grapevine still lists it, how many live Mini rows nodeIndexs has by owner, and whether sessions still claim on anythink's orgs — not yet run, the Mini is not yet upgraded.
---

# Mini owner reading

For goal `the-mini-serves-its-enterprise-and-nothing-else`: once the
Mini is upgraded to a binary that registers with `owner: enterprise,
enterprise: anythink` and re-registers, three things need reading —
whether `grapevine` (an org of Chris's, no enterprise's) still lists
the Mini, how many live Mini rows `nodeIndexs` has by owner and their
`ownerKind`, and whether sessions still claim on anythink's orgs.
Before this, answering any of the three took a person reading the
console or the gateway by hand. `scripts/mini-owner-reading.sh` runs
all three as one command; see its own `--help` for the exact reads.

## Run it

```sh
RUNSHEET_TOKEN=<an rs_* console token, or the Mini's ~/.mesh/token bearer> \
  scripts/mini-owner-reading.sh
```

That uses the defaults — gateway `https://api.runsheet.dev/graphql`,
slug `runsheet-mini`, the grapevine org id resolved from `myOrgs` by
tenant slug or login `grapevine`. To pin the grapevine org id instead
of resolving it (faster, and works on a token that is not a member of
grapevine):

```sh
RUNSHEET_TOKEN=<token> scripts/mini-owner-reading.sh \
  https://api.runsheet.dev/graphql runsheet-mini <grapevine-org-id>
```

Read the grapevine org id with `mesh node status` on the Mini, or ask
the gateway for `myOrgs` yourself — the script does the same lookup
when the id is omitted. It prints three lines:

```
grapevine: <n> live Mini row(s)
by owner: <n> live row(s), ownerKind <kind>, ownerId <id>
claims on anythink orgs: <n> in the last 24h
```

The test (`scripts/mini-owner-reading_test.sh`, fixture-driven, needs
no gateway or token) is run with:

```sh
scripts/mini-owner-reading_test.sh
```

## Reading

not yet run: the Mini has not been upgraded to a binary that registers
with `owner: enterprise, enterprise: anythink` (items 2, 3 and 4 of
this goal are not yet merged), so re-registering it now would still
run the legacy per-org path this item exists to read past. Recorded
2026-10-03.
