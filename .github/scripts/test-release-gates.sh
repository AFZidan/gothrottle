#!/usr/bin/env bash
# Tests for the release gate scripts.
#
# These run in CI (the "Lint Workflows" job) and locally via `make test-scripts`.
# They exist because the release workflow's gates are shell, and shell that is
# never executed against its failure cases is not a gate — every case below is one
# the audit found accepted when it should have been rejected, or rejected when it
# should have been accepted.
set -uo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
version_script="${script_dir}/check-version.sh"
classify_script="${script_dir}/classify-checks.sh"
module=github.com/AFZidan/gothrottle

failures=0
checks=0

pass() { printf '  ok   %s\n' "$1"; }
fail() {
  printf '  FAIL %s\n' "$1" >&2
  failures=$((failures + 1))
}

# expect_version <accept|reject> <version> [module]
expect_version() {
  local want=$1 version=$2 mod=${3-}
  checks=$((checks + 1))
  local output status
  output=$("$version_script" "$version" "$mod" 2>&1)
  status=$?
  case "$want" in
    accept)
      if [ "$status" -eq 0 ]; then
        pass "accepts '$version'${mod:+ for $mod}"
      else
        fail "rejects '$version'${mod:+ for $mod} but should accept: $output"
      fi
      ;;
    reject)
      if [ "$status" -ne 0 ]; then
        pass "rejects '$version'${mod:+ for $mod}"
      else
        fail "accepts '$version'${mod:+ for $mod} but should reject"
      fi
      ;;
  esac
}

# expect_checks <pass|fail> <description> <tsv> [own-suite]
expect_checks() {
  local want=$1 description=$2 tsv=$3 own=${4-}
  checks=$((checks + 1))
  local output status
  output=$(printf '%s' "$tsv" | "$classify_script" "$own" 2>&1)
  status=$?
  case "$want" in
    pass)
      if [ "$status" -eq 0 ]; then
        pass "$description"
      else
        fail "$description — gate rejected a commit it should accept: $output"
      fi
      ;;
    fail)
      if [ "$status" -ne 0 ]; then
        pass "$description"
      else
        fail "$description — gate accepted a commit it should reject"
      fi
      ;;
  esac
}

echo "check-version.sh"

# Canonical versions.
expect_version accept v1.1.1
expect_version accept v0.0.1
expect_version accept v10.20.30
expect_version accept v1.2.3-alpha
expect_version accept v1.2.3-alpha.1
expect_version accept v1.2.3-0.20240101120000-abcdef123456
expect_version accept v1.2.3-rc.1+build.5
expect_version accept v1.2.3+build.01

# Shape.
expect_version reject ''
expect_version reject 1.2.3
expect_version reject v1.2
expect_version reject v1.2.3.4
expect_version reject v1.2.x
expect_version reject 'v1.2.3 '
expect_version reject 'v1.1.1; rm -rf /'

# Leading zeroes in the numeric core.
expect_version reject v01.2.3
expect_version reject v1.02.3
expect_version reject v1.2.03

# The cases the audit named: empty and zero-padded pre-release identifiers.
expect_version reject v1.2.3-01
expect_version reject v1.2.3-alpha..1
expect_version reject v1.2.3-.
expect_version reject v1.2.3-
expect_version reject v1.2.3+
expect_version reject v1.2.3+build..1
expect_version reject 'v1.2.3-alpha_1'

# A numeric pre-release identifier of exactly zero is legal; 00 is not.
expect_version accept v1.2.3-0
expect_version reject v1.2.3-00

# +incompatible is a resolver artifact, not a publishable tag.
expect_version reject v2.0.0+incompatible

# Module major-version suffix compatibility.
expect_version accept v1.1.1 "$module"
expect_version accept v0.5.0 "$module"
expect_version reject v2.0.0 "$module"
expect_version reject v10.0.0 "$module"
expect_version accept v2.0.0 "${module}/v2"
expect_version accept v10.0.0 "${module}/v10"
expect_version reject v1.1.1 "${module}/v2"

echo
echo "classify-checks.sh"

required_names=(
  'Test (1.19, 6)' 'Test (1.19, 7)' 'Test (1.20, 6)' 'Test (1.20, 7)'
  'Test (1.21, 6)' 'Test (1.21, 7)' 'Test (1.22, 6)' 'Test (1.22, 7)'
  'Lint' 'Lint Workflows' 'Security Scan' 'Build' 'Analyze (go)'
)

# all_green builds a full, healthy check list, optionally overriding one entry.
# override_name/override_status/override_conclusion are read from the environment.
all_green() {
  local name
  for name in "${required_names[@]}"; do
    if [ -n "${override_name:-}" ] && [ "$name" = "${override_name}" ]; then
      printf '100\t%s\t%s\t%s\n' "$name" "${override_status}" "${override_conclusion}"
    else
      printf '100\t%s\tcompleted\tsuccess\n' "$name"
    fi
  done
}

# drop_check builds the full list minus one name.
drop_check() {
  local drop=$1 name
  for name in "${required_names[@]}"; do
    [ "$name" = "$drop" ] && continue
    printf '100\t%s\tcompleted\tsuccess\n' "$name"
  done
}

expect_checks pass "accepts a commit with every required check green" "$(override_name= all_green)"

# Empty input: the case a nonempty-list gate silently accepted.
expect_checks fail "rejects a commit with no check runs at all" ''

# Missing required checks, one at a time.
for name in "${required_names[@]}"; do
  expect_checks fail "rejects a commit missing '$name'" "$(drop_check "$name")"
done

# A skipped required job verified nothing.
expect_checks fail "rejects a skipped required check" \
  "$(override_name='Test (1.22, 7)' override_status=completed override_conclusion=skipped all_green)"

# Still running.
expect_checks fail "rejects an in-progress required check" \
  "$(override_name=Lint override_status=in_progress override_conclusion=pending all_green)"
expect_checks fail "rejects a queued required check" \
  "$(override_name=Build override_status=queued override_conclusion=pending all_green)"

# Failure conclusions.
for conclusion in failure cancelled timed_out action_required stale neutral; do
  expect_checks fail "rejects a required check concluded '$conclusion'" \
    "$(override_name='Security Scan' override_status=completed override_conclusion="$conclusion" all_green)"
done

# Duplicates: a re-run adds a second check run with the same name. Every
# occurrence must be healthy, so a green re-run does not paper over a red original.
expect_checks pass "accepts duplicated required checks when all are green" \
  "$(override_name= all_green; printf '100\tLint\tcompleted\tsuccess\n')"
expect_checks fail "rejects a duplicated required check where one occurrence failed" \
  "$(override_name= all_green; printf '100\tLint\tcompleted\tfailure\n')"

# Extra non-required checks are ignored rather than being treated as required.
expect_checks pass "ignores an unrelated failing non-required check" \
  "$(override_name= all_green; printf '100\tsomebody-elses-bot\tcompleted\tfailure\n')"

# Suite exclusion: the release run's own in-progress job must not block itself,
# and excluding a suite must not excuse a required check that lives in it.
expect_checks pass "excludes the release workflow's own check suite" \
  "$(override_name= all_green; printf '999\tVerify release candidate\tin_progress\tpending\n')" 999
expect_checks fail "excluding a suite does not excuse a required check inside it" \
  "$(drop_check Lint; printf '999\tLint\tcompleted\tsuccess\n')" 999

echo
if [ "$failures" -gt 0 ]; then
  printf '%d of %d checks failed\n' "$failures" "$checks" >&2
  exit 1
fi
printf 'all %d checks passed\n' "$checks"
