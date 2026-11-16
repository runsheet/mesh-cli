---
summary: When a checkout has no .runsheet/validate.yml, run_suite no longer refuses. The node infers the suites from the repository itself (internal/validate/infer). go.mod and go.work give go-build, go-vet and go-test. A package.json whose package manager is named by its packageManager field or a lockfile gives node-install plus its own lint, typecheck, test and build scripts. When neither gives a test, a root Makefile's test and check targets are used. The inferred suites are written as a validate.yml and read back through validate.Parse. An explicit validate.yml always wins, as a whole file. The answer, the summary, the signed record's blob and the node's log name what was inferred and from which files, so a product can store the profile later. Amended 2026-11-16: a repository that holds only documents infers one suite, docs, whose command `mesh docs-check` is the node's built-in docs check, run in process. Amends ADR-013.
status: accepted
---

# ADR-053: A repo with no validate.yml is validated from what the node finds in it

2026-11-16. Mesh v1. This applies to `internal/validate/infer` (new),
`internal/nodetools` (runsuite.go) and `internal/nodev1` (tools.go,
logging only). It amends ADR-013, which said "a repository with no such
file is `validate.ErrNoValidation`". It comes from runsheet goal
`mesh-works-out-how-to-build-and-test-a-repo` (direction rule 7) and is
the goal's first slice.

## Context

Until now, a v1 validate lease on a repository without
`.runsheet/validate.yml` ended in run_suite's refusal: "the repository
declares no validation". Every repository therefore had to carry the
file before Mesh would validate its first pull request. On 14 November
the owner asked that "the Mesh and runsheet configs should be very basic
or not needed at all." The goal's acceptance test is a Go repository and
a pnpm repository, each enrolled with no Mesh or runsheet files, both
validating on their first pull request.

## Decision

**With no validate.yml, the node infers the suites.**
`infer.Load(dir)` returns the checkout's validate.yml when it has one.
Otherwise `infer.Infer` reads the checkout and applies these rules:

- **Go.** A module is the root's `go.mod`, any `use` of a root
  `go.work` (when it stays inside the repository), or a first-level
  directory that holds a `go.mod`. Every module gets `go-build`
  (`go build ./...`, kind build) and `go-vet` (`go vet ./...`, kind
  lint). When any module has a `_test.go` of its own, every module also
  gets `go-test` (`go test -json ./...`, kind unit, `results: stdout`,
  15m). A repository with only a root module runs each command once at
  the root. With more modules, the command runs in each module's
  directory, and the suite is red if any module fails. `go-test` reads
  the supervisor's `MESH_SUITE_TESTS` as `-run=^(A|B)$`, so a retry by
  name re-runs only the failing tests. A root-only module also honours
  `MESH_SUITE_TARGETS`. Every command puts the usual Go directories on
  PATH, as the estate's own validate.yml files do.
- **Node.** This rule reads the root's `package.json`. Only when the
  root has none does it read each first-level directory's
  `package.json`. The package manager comes from the `packageManager`
  field (pnpm, npm or yarn), or else from a lockfile beside the
  package.json (`pnpm-lock.yaml`, `package-lock.json`, `yarn.lock`).
  That gives `node-install`, which installs from the lockfile:
  `pnpm install --frozen-lockfile`, `npm ci`, `yarn install --immutable`
  (yarn 2 or later, or a `.yarnrc.yml`) or
  `yarn install --frozen-lockfile`. It is followed by `node-lint`,
  `node-typecheck`, `node-test` and `node-build`, each running
  `<pm> run <script>` with `CI=true`. A script gets a suite only when
  package.json defines it; the node never makes one up, and it skips
  npm's `"no test specified"` placeholder. A package in a first-level
  directory gets suites with the directory as a suffix
  (`node-test-web`), and their commands `cd` into it.
- **Makefile.** This rule applies only when neither of the above gives a
  test (a unit suite). It reads the root `GNUmakefile`, `makefile` or
  `Makefile`, in make's own order, and uses only the targets named
  exactly `test` or `check` (`make-test`, `make-check`, kind unit).
- **Nothing recognised** returns `infer.ErrNothingToInfer`. run_suite
  then refuses with "the suites cannot run: … looked for go.mod or
  go.work; package.json with a packageManager field or a lockfile …; a
  Makefile with a target named test or check …". The refusal also lists
  what the node found and passed over, for example a package.json with
  no package manager.

Every inferred suite asks for 4 CPUs and 8g, which is what each lane
held before suites could ask for their own room (ADR-049 amendment;
runsheet's own validate.yml names the same). No inferred suite names a
group, shards or paths. So they all run in one lane, in the lease's own
pod, one after another in the order above, and every change runs every
suite.

**The inference is a validate.yml.** `Infer` writes the suites as a YAML
file (`Profile.File`, under a header naming the files it was inferred
from) and reads that file back through `validate.Parse`. The run is then
exactly what committing that file would run: the same defaults
(retries 1, stall 5m), the same `min_tests` floor rules and the same
`SuiteHash` (`Profile.SuiteHash`). Committing the file as
`.runsheet/validate.yml` is how a repository overrides the inference.

**An explicit validate.yml wins whole.** When the file exists, the
inference never runs. Nothing is merged and no inferred suite is added
alongside a declared one. A validate.yml that does not parse returns its
own error, never an inference in its place.

**What was inferred is reported.** When a run is inferred, run_suite
reports it in four places:

- its answer carries `inferred` (`from`, `suites`, `notes`,
  `validate_yml`, `suite_hash`);
- the summary begins "no .runsheet/validate.yml: inferred suites …
  from …";
- the signed record's output blob (`SuiteOutput.Inferred`) carries the
  same profile, and its input digest covers the profile's suite hash;
- the node logs `run_suite: no validate.yml: inferred suites go-build,
  go-vet, go-test from go.mod` against the lease.

A product can take the profile from the blob and store it. This slice
does not store it in Mesh, and the API is unchanged.

## Not handled (later slices)

- **Storing what was learned.** The node infers again on every run. It
  neither stores nor reads a profile from Mesh.
- **A small override file.** The only override is a whole validate.yml.
  There is no partial file that changes one inferred suite.
- **Deeper monorepos.** The node does not look for Go modules below the
  first level unless go.work names them, and it ignores `go.work` files
  in subdirectories. It does not read pnpm, npm or yarn workspaces
  member by member: a root package.json's scripts are taken to cover its
  workspace. Turborepo and Nx are not detected, except through the
  scripts they put in package.json. Makefiles in subdirectories are not
  read.
- **Other ecosystems.** Python, Rust, Java, Ruby, bun (`bun.lockb`) and
  Deno are not recognised.
- **Services and environment.** Nothing infers `.mesh/services.yml`,
  `env` or a Postgres. A repository whose tests need a database still
  declares it.
- **Toolchains.** pnpm and yarn must already be on the image's PATH;
  the node does not run `corepack enable`.
- **Budget and lane reservation.** `suiteBudget` and `reserveLanes` in
  internal/nodev1 still read only a declared validate.yml. An inferred
  run has no budget, and since it runs in a single lane, the eager lane
  reservation has nothing to reserve.
- **The platform's own enrolment check.** A product that holds an
  evaluation because the repository has no validate.yml (runsheet's
  unenrolled-repo hold) still holds it. Changing that is the product's
  side of the goal.

## Amendment 2026-11-16: a repository that holds only documents is validated by a docs check

**Context.** The rules above recognise code: a handbook or a specs
repository has none, so it fell through to `ErrNothingToInfer` and every
validation of it was handed back. On 16 November dl360 handed back 27
leases of `GrapevineNetwork/business-portal-handbook` ("is a
documentation-only repository", "Repository declares no validation
(.runsheet/validate.yml)"), as both product and cannot_run, across nine
merge chains each handed back about five times before it was held, and
each chain formed again. Those pull requests could never go green, and
every offer cost a lease (runsheet goal
`a-docs-only-repo-is-validated-by-a-docs-check`).

**Decision.** A fourth rule, applied only when none of the three above
gives any suite:

- **Documents.** A checkout holds only documents
  (`infer.DocumentsOnly`) when every file outside a dot-path — any path
  with a segment starting with `.`, so `.git`, `.github`, `.runsheet`,
  `.gitkeep` — is a document or an asset, and at least one is Markdown.
  A document is a file whose extension, in any case, is one of
  `infer.DocumentExts` (`.md .markdown .mdx .txt .rst .adoc`) or
  `infer.AssetExts` (`.png .jpg .jpeg .gif .svg .webp .ico .pdf`), or an
  extensionless file named, in any case, one of `infer.DocumentNames`
  (`LICENSE LICENCE NOTICE AUTHORS CODEOWNERS COPYING README`); a
  symlink is judged by its own name and never followed. The list is an
  allowlist on purpose: one file of code (a `.py`, a `.sh`, a `.json`, a
  Dockerfile) makes the repository not documents-only, so nothing that
  holds code is ever checked with less than its code would need. Such a
  checkout still answers `ErrNothingToInfer`, its error naming the file
  that is not a document.
- It infers one suite, `docs` (kind lint, the same room as every
  inferred suite), whose command is exactly `mesh docs-check`, and its
  profile is "inferred suites docs from N Markdown files".

**The check** (`internal/validate/docscheck`). Over every `.md` and
`.markdown` file outside a dot-path: the file is UTF-8 with no NUL byte
and no larger than 4 MiB; a frontmatter block (a first line `---`)
closes and is a YAML mapping; every fenced code block (``` or ~~~)
closes; and every relative link or image — `[text](target)`,
`![alt](target)`, a reference definition `[id]: target` — outside code
fences and inline code names a file or directory inside the repository,
its `#fragment` and `?query` cut and URL-decoded, `/x` read from the
root. A target that climbs out of the repository is a problem of its
own; one with a scheme, `//host` or only a fragment is not checked. An
`.mdx` file has only its encoding and frontmatter read (its body is
JSX). It reports `path:line: what` per problem and a summary.

**It runs in process.** run_suite runs a step whose command is exactly
`mesh docs-check` (or `mesh docs-check .`) itself, on the checkout,
rather than through the lease's runner (nodetools `withBuiltins`): the
pod image carries no mesh binary. Every other step still runs in the
pod, the results in the file's order. This is node code, not the
session's: it reads the checkout through an `os.Root`, so a symlink the
session wrote never makes it read outside the checkout, caps every file
it reads, and executes nothing. Each document it read is one test of the
step (package `docs`, test the path), passing, or failing with its
problems as output, so a red check names the documents wherever a
failing test is named. `mesh docs-check [dir]` runs the same check
anywhere mesh is installed, so committing the inferred file as
`.runsheet/validate.yml` means the same thing.

**runsheet's side.** runsheet's merge queue holds the same
documents-only rule (the same lists) to tell a repository whose chain it
offers a validation (documents-only: this check proves it) from one with
no validate.yml that holds code, which it holds once for a person rather
than offering a validation no node can reach a verdict on. The two lists
change together; `TestDocumentsOnlyListsArePinned` pins this side.

**Not handled.** Anchors (`#section`) are not checked against headings,
links in raw HTML are not read, and prose is not reviewed.

