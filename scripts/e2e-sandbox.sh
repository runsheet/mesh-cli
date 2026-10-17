#!/usr/bin/env bash
# e2e-sandbox.sh — the sandboxed-sessions e2e on this machine (docs/e2e-sandbox.md).
#
#   1. makes the node image present the way a node does before its drain
#      (SandboxConfig.EnsureImage): built here from the embedded
#      Containerfile (`sandbox.image: build`, the default) or pulled;
#   2. runs every sandbox e2e test with MESH_SANDBOX_E2E=1: the probe
#      session, the sidecars, and the node's own build and evaluation on
#      runsheet's Postgres and the Redis fixture;
#   3. runs a runsheet item's evaluation on Codex in a pod from the node
#      image (TestCodexEvaluationInAPodE2E) on the Codex credential given
#      below — skipped, and said so, when none is — and shows podman holds
#      no pod of it after;
#   4. checks that no session still uses latchkey-e2e-pg, and prints the
#      command that removes it. It never removes it itself.
#
# Environment:
#   MESH_SANDBOX_ENSURE        build (default), pull, or skip
#   MESH_SANDBOX_IMAGE         the image (default ghcr.io/runsheet/mesh-node:dev)
#   MESH_SANDBOX_KEEP_VERSION  the running node's version, whose image the
#                              prune keeps (default: `mesh --version`, else none)
#   MESH_HOME                  the node's directory (default ~/.mesh)
#   MESH_E2E_CODEX_AUTH        a ChatGPT login's auth.json, the issued Codex
#                              credential the Codex case runs on (a copy:
#                              Codex may refresh it, and the e2e keeps the
#                              refresh in its own cache dir), or
#   MESH_E2E_CODEX_API_KEY     an OpenAI API key instead
#
# Exit status: 0 when the e2e passes and nothing uses latchkey-e2e-pg,
# 1 when the e2e fails, 2 when something still uses it.
set -euo pipefail

cd "$(dirname "$0")/.."

ensure=${MESH_SANDBOX_ENSURE:-build}
image=${MESH_SANDBOX_IMAGE:-ghcr.io/runsheet/mesh-node:dev}
mesh_home=${MESH_HOME:-$HOME/.mesh}
legacy=latchkey-e2e-pg

if ! command -v podman >/dev/null 2>&1; then
  echo "e2e-sandbox: podman is not installed; the sandbox e2e needs rootless podman" >&2
  exit 1
fi
echo "== podman: $(podman --version)"

keep=${MESH_SANDBOX_KEEP_VERSION:-}
if [[ -z $keep ]]; then
  keep=$(mesh --version 2>/dev/null | awk '{print $NF}') || true
  keep=${keep:-none}
fi

# 1. the image
case $ensure in
  build|pull)
    echo "== node image: $ensure $image (keeping the image of $keep)"
    MESH_SANDBOX_E2E=1 MESH_SANDBOX_ENSURE=$ensure MESH_SANDBOX_IMAGE=$image \
      MESH_SANDBOX_KEEP_VERSION=$keep \
      go test -count=1 -v -timeout 40m -run '^TestEnsureImageE2E$' ./internal/node
    ;;
  skip)
    podman image exists "$image" || { echo "e2e-sandbox: $image is not on this machine" >&2; exit 1; }
    echo "== node image: $image (already here)"
    ;;
  *)
    echo "e2e-sandbox: MESH_SANDBOX_ENSURE=$ensure: want build, pull or skip" >&2
    exit 1
    ;;
esac

# 2. the e2e
echo "== e2e: MESH_SANDBOX_E2E=1 MESH_SANDBOX_IMAGE=$image"
MESH_SANDBOX_E2E=1 MESH_SANDBOX_IMAGE=$image \
  go test -count=1 -v -timeout 30m -run 'E2E' -skip '^TestCodexEvaluationInAPodE2E$' ./internal/sandbox ./internal/node

# 3. a runsheet evaluation on Codex in a pod
if [[ -z ${MESH_E2E_CODEX_AUTH:-} && -z ${MESH_E2E_CODEX_API_KEY:-} ]]; then
  echo "== codex: skipped — no Codex credential given; set MESH_E2E_CODEX_AUTH=<auth.json> or MESH_E2E_CODEX_API_KEY to run it"
else
  echo "== codex: a runsheet evaluation on Codex in a pod from $image"
  out=$(mktemp)
  set +e
  MESH_SANDBOX_E2E=1 MESH_SANDBOX_IMAGE=$image \
    go test -count=1 -v -timeout 20m -run '^TestCodexEvaluationInAPodE2E$' ./internal/node 2>&1 | tee "$out"
  status=${PIPESTATUS[0]}
  set -e
  if (( status != 0 )) || grep -q -- '--- SKIP' "$out"; then
    rm -f "$out"
    echo "e2e-sandbox: the Codex evaluation in a pod did not pass" >&2
    exit 1
  fi
  echo "== codex: the run's usage record and verdict"
  grep -E 'usage record:|verdict recorded:' "$out" | sed 's/^ *//'
  rm -f "$out"
  left=$(podman pod ls --format '{{.Name}}' | grep -E '^mesh-e2e-codex-' || true)
  echo "== codex: podman pod ls after: ${left:-no mesh-e2e-codex- pod}"
  [[ -z $left ]] || { echo "e2e-sandbox: pods left: $left" >&2; exit 1; }
fi

# 4. latchkey-e2e-pg
echo "== who still uses $legacy"
in_use=0
localpg='(localhost|127\.0\.0\.1|\[::1\]|0\.0\.0\.0):5432'
# DATABASE_URL, or any postgres URL (runsheet's RUNSHEET_TEST_PG), on it
pgenv="(DATABASE_URL=.*|postgres(ql)?://[^[:space:]]*@)$localpg"

# containers publishing :5432, podman's and docker's
for rt in podman docker; do
  command -v "$rt" >/dev/null 2>&1 || continue
  rows=$("$rt" ps -a --format '{{.Names}}	{{.Status}}	{{.Ports}}' 2>/dev/null | grep -E ':5432->' || true)
  [[ -z $rows ]] && continue
  while IFS=$'\t' read -r name status ports; do
    note=""
    [[ $name == "$legacy" ]] && note="  <- the one to retire"
    echo "  $rt container on :5432: $name ($status) $ports$note"
  done <<<"$rows"
done

# a repository declares its services when it has .mesh/services.yml or a
# workflow with services:; its sessions then get their own Postgres
declares_services() {
  local repo=$1
  [[ -f $repo/.mesh/services.yml || -f $repo/.mesh/services.yaml ]] && return 0
  compgen -G "$repo/.github/workflows/*.y*ml" >/dev/null || return 1
  grep -qE '^[[:space:]]+services:' "$repo"/.github/workflows/*.y*ml
}

# node.yaml
if [[ -f $mesh_home/node.yaml ]] && grep -nE "DATABASE_URL.*$localpg|postgres(ql)?://[^[:space:]]*@$localpg" "$mesh_home/node.yaml"; then
  echo "  $mesh_home/node.yaml points a session at the host's :5432"
  in_use=1
fi

# the workspaces' env files, in checkouts that declare no services
if [[ -d $mesh_home/work ]]; then
  while IFS= read -r -d '' f; do
    repo=$(dirname "$f")
    if grep -qE "$pgenv" "$f"; then
      if declares_services "$repo"; then
        echo "  $f: localhost:5432, served by the session's own sidecar"
      else
        echo "  $f: a database on the host's :5432 and no sidecar declared"
        in_use=1
      fi
    fi
  done < <(find "$mesh_home/work" -mindepth 2 -maxdepth 4 \( -name .env -o -name '.env.*' -o -name .envrc \) -type f -print0 2>/dev/null)
fi

# running host sessions (sandbox: off) whose environment names it
for env in /proc/[0-9]*/environ; do
  pid=${env#/proc/}; pid=${pid%/environ}
  [[ -r $env ]] || continue
  cmd=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)
  [[ $cmd == *claude* || $cmd == *codex* ]] || continue
  if tr '\0' '\n' < "$env" 2>/dev/null | grep -qE "$pgenv"; then
    echo "  pid $pid ($cmd): a database on the host's :5432 in its environment"
    in_use=1
  fi
done

rm_cmd="podman rm -f $legacy"
if ! podman container exists "$legacy" 2>/dev/null; then
  rm_cmd="docker rm -f $legacy"
fi
if (( in_use )); then
  echo "== $legacy is still used (above); do not remove it yet. Once nothing is, it goes with:"
  echo "   $rm_cmd"
  exit 2
fi
echo "== nothing uses $legacy. Remove it by hand with:"
echo "   $rm_cmd"
