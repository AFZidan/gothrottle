#!/usr/bin/env bash
# Classify a commit's check runs against the required set. Pure logic: it reads
# check runs from stdin and writes a verdict, so it can be tested without GitHub.
#
# Usage: classify-checks.sh [own-check-suite-id] < checks.tsv
#
# Input is one check run per line, tab-separated: suite-id, name, status,
# conclusion. check-required-checks.sh produces exactly that from the API.
#
# Fail-closed, which is the point of the gate: the required set is named here and a
# missing check is a failure. "Every check that happens to exist is green" is not a
# gate — a commit whose CI never started has an empty list, and an empty list
# contains no failures.
#
# A required check must be `completed` with conclusion `success`. `skipped` is
# rejected: a required job that did not run verified nothing, and the usual cause
# is a conditional or path filter that quietly excluded it.
#
# Duplicates are expected — a re-run adds a second check run with the same name —
# and every occurrence must be healthy. The latest being green is not sufficient on
# its own, because "green after N attempts" and "green" are different claims about
# a commit, and only the release gate is in a position to insist on the latter.
#
# Only the release workflow's own check suite is excluded, by id: when the commit
# being released is the tip of main, that job is in the list as in_progress and
# would otherwise block itself.
set -euo pipefail

own_suite=${1-}

# The required set. Adding a job to ci.yml means adding it here; the reverse holds
# too, since a name that never appears fails the gate rather than being ignored.
# Documented in README "Releasing".
required_checks=(
  'Test (1.19, 6)'
  'Test (1.19, 7)'
  'Test (1.20, 6)'
  'Test (1.20, 7)'
  'Test (1.21, 6)'
  'Test (1.21, 7)'
  'Test (1.22, 6)'
  'Test (1.22, 7)'
  'Lint'
  'Lint Workflows'
  'Security Scan'
  'Build'
  'Analyze (go)'
)

all=$(cat)
others=$(printf '%s\n' "$all" | awk -F'\t' -v own="${own_suite:-}" 'NF && $1 != own')

if [ -z "$others" ]; then
  echo "::error::no CI check runs found for this commit; CI must run on a commit before it is released" >&2
  exit 1
fi

echo "Check runs considered:"
printf '%s\n' "$others" | awk -F'\t' '{printf "  %s: %s/%s\n", $2, $3, $4}' | sort

missing=()
unhealthy=()
for name in "${required_checks[@]}"; do
  matches=$(printf '%s\n' "$others" | awk -F'\t' -v want="$name" '$2 == want')
  if [ -z "$matches" ]; then
    missing+=("$name")
    continue
  fi
  bad=$(printf '%s\n' "$matches" | awk -F'\t' 'NF && ($3 != "completed" || $4 != "success")')
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    unhealthy+=("$(printf '%s' "$line" | awk -F'\t' '{printf "%s: %s/%s", $2, $3, $4}')")
  done <<EOF
$bad
EOF
done

status=0
if [ ${#missing[@]} -gt 0 ]; then
  echo "::error::missing required check runs:" >&2
  printf '  %s\n' "${missing[@]}" >&2
  status=1
fi
if [ ${#unhealthy[@]} -gt 0 ]; then
  echo "::error::required check runs that did not succeed:" >&2
  printf '  %s\n' "${unhealthy[@]}" >&2
  status=1
fi
[ "$status" -eq 0 ] || exit 1

echo "OK: all ${#required_checks[@]} required checks succeeded"
