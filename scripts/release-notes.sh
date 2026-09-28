#!/usr/bin/env bash
# Prints the release notes for <tag>: the PRs merged since <previous-tag>,
# grouped by goal slug (from the branch mesh/<slug>-<seq>) with the goal
# doc's link, in merge order. A PR from another branch lands under
# "Other"; a commit with no PR (squash subject ending "(#NNN)", or a
# true merge's "Merge pull request #NNN") lands under "Commits".
#
# Each PR's title and headRefName come from `gh pr view NNN --json
# number,title,headRefName,url`, or — so this needs no network — from the
# file named by $RELEASE_NOTES_PRS, a JSON array of
# {number,title,headRefName,url}.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 <previous-tag> <tag>" >&2
  exit 1
fi

PREV_TAG="$1"
TAG="$2"

sha="$(git rev-parse "$TAG")"

echo "Release ${TAG} from \`${sha}\`. SHA256SUMS is signed keyless with Sigstore by \`.github/workflows/release-tag.yml\`; see the README's Release section to verify."
echo

pr_json() {
  local number="$1"
  if [ -n "${RELEASE_NOTES_PRS:-}" ]; then
    jq -c --argjson n "$number" '.[] | select(.number == $n)' "$RELEASE_NOTES_PRS"
  else
    gh pr view "$number" --json number,title,headRefName,url 2>/dev/null || true
  fi
}

slugs_order=()
declare -A slug_lines
other_lines=()
commit_lines=()

while IFS=$'\t' read -r sha1 subject; do
  [ -n "$sha1" ] || continue

  number=""
  if [[ "$subject" =~ \(#([0-9]+)\)[[:space:]]*$ ]]; then
    number="${BASH_REMATCH[1]}"
  elif [[ "$subject" =~ Merge\ pull\ request\ \#([0-9]+) ]]; then
    number="${BASH_REMATCH[1]}"
  fi

  if [ -z "$number" ]; then
    short="$(git rev-parse --short "$sha1")"
    commit_lines+=("- ${subject} (${short})")
    continue
  fi

  pr="$(pr_json "$number")"
  if [ -z "$pr" ]; then
    short="$(git rev-parse --short "$sha1")"
    commit_lines+=("- ${subject} (${short})")
    continue
  fi

  title="$(jq -r '.title' <<<"$pr")"
  branch="$(jq -r '.headRefName' <<<"$pr")"
  line="- ${title} (#${number})"

  if [[ "$branch" =~ ^mesh/(.+)-[0-9]+$ ]]; then
    slug="${BASH_REMATCH[1]}"
    if [ -z "${slug_lines[$slug]+x}" ]; then
      slugs_order+=("$slug")
      slug_lines[$slug]=""
    fi
    slug_lines[$slug]+="${line}"$'\n'
  else
    other_lines+=("$line")
  fi
done < <(git log --first-parent --reverse --pretty=format:'%H%x09%s' "${PREV_TAG}..${TAG}"; echo)

for slug in "${slugs_order[@]}"; do
  echo "### ${slug}"
  echo
  echo "Goal doc: https://github.com/runsheet/mesh-cli/blob/main/specs/goal-${slug}.md"
  echo
  printf '%s' "${slug_lines[$slug]}"
  echo
done

if [ "${#other_lines[@]}" -gt 0 ]; then
  echo "### Other"
  echo
  printf '%s\n' "${other_lines[@]}"
  echo
fi

if [ "${#commit_lines[@]}" -gt 0 ]; then
  echo "### Commits"
  echo
  printf '%s\n' "${commit_lines[@]}"
fi
