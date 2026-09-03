#!/usr/bin/env bash
# Assert that every required CI check for a commit completed successfully, and that
# any classic commit statuses are green.
#
# Usage: check-required-checks.sh <sha> [own-check-suite-id]
#
# This is the GitHub-facing half; the classification logic lives in
# classify-checks.sh so it can be tested without the API. Requires gh and jq, and
# reads GITHUB_REPOSITORY or falls back to the current repository.
set -euo pipefail

sha=${1-}
own_suite=${2-}
repo=${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq '.nameWithOwner')}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

fail() {
  echo "::error::$*" >&2
  exit 1
}

[ -n "$sha" ] || fail "no commit SHA supplied"

# Tab-separated: suite id, name, status, conclusion.
gh api --paginate "repos/${repo}/commits/${sha}/check-runs?per_page=100" \
  --jq '.check_runs[] | [(.check_suite.id | tostring), .name, .status, (.conclusion // "pending")] | @tsv' \
  > /tmp/release-check-runs.tsv

echo "Commit $sha:"
"${script_dir}/classify-checks.sh" "${own_suite:-}" < /tmp/release-check-runs.tsv

# Classic commit statuses are a separate mechanism from check runs, and some tools
# still use them. When any exist, the combined state must be success. A repository
# with no status-posting tools reports state=pending with total_count=0, and only
# that shape is accepted as "there are none".
status_json=$(gh api "repos/${repo}/commits/${sha}/status" --jq '{state: .state, total: .total_count}')
state=$(printf '%s' "$status_json" | jq -r '.state')
total=$(printf '%s' "$status_json" | jq -r '.total')
echo "Combined commit status: $state ($total status(es))"

if [ "$total" -gt 0 ] && [ "$state" != "success" ]; then
  gh api "repos/${repo}/commits/${sha}/status" --jq '.statuses[] | "  \(.context): \(.state)"' >&2 || true
  fail "commit $sha has $total classic commit status(es) with combined state '$state', want 'success'"
fi
if [ "$total" -eq 0 ] && [ "$state" != "pending" ] && [ "$state" != "success" ]; then
  fail "commit $sha reports combined status '$state' with no statuses, which is not a shape this gate accepts"
fi

echo "OK: required checks and commit statuses are clean for $sha"
