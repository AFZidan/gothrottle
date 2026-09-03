#!/usr/bin/env bash
# Validate a release version as a canonical, Go-compatible semantic version tag,
# and confirm the module path can carry its major version.
#
# Usage: check-version.sh <version> [module-path]
#
# Go's module system is stricter than the semver spec's grammar allows, and it is
# unforgiving: a tag it considers non-canonical is not a version at all, so
# `go get module@vX.Y.Z` fails while the tag sits there looking fine. Every rule
# below is a rule the module system actually enforces.
#
# Rejected beyond plain semver:
#   - a missing leading `v`, or fewer than three numeric fields
#   - leading zeroes in any numeric field, including numeric pre-release
#     identifiers (v1.2.3-01)
#   - an empty pre-release or build identifier (v1.2.3-, v1.2.3-alpha..1, v1.2.3+)
#   - a `+incompatible` suffix, which is a resolver artifact and not something to
#     publish
#
# Accepted: v1.2.3, v1.2.3-alpha.1, v1.2.3-rc.1+build.5, v0.0.1, v10.20.30.
#
# The accept/reject set was cross-checked against golang.org/x/mod/semver and
# agrees with it everywhere except two deliberate divergences, both stricter:
#
#   - v1.2 is IsValid per x/mod (it canonicalizes to v1.2.0), but it is not
#     canonical, and a published tag has to be the canonical form or `go get` at
#     that exact string fails.
#   - v2.0.0+incompatible is IsValid, but +incompatible is something the resolver
#     synthesizes for a pre-modules major version. Publishing it as a tag is
#     always a mistake.
set -euo pipefail

version=${1-}
module=${2-}

fail() {
  echo "::error::$*" >&2
  exit 1
}

[ -n "$version" ] || fail "no version supplied"

# Split off build metadata first, then pre-release, leaving the numeric core.
# has_prerelease/has_build record that a separator was present, because an empty
# value after the separator is itself invalid: v1.2.3- and v1.2.3+ must be
# rejected, and testing the value alone cannot tell them from v1.2.3.
core=$version
build=
prerelease=
has_build=false
has_prerelease=false
case "$core" in
  *+*)
    has_build=true
    build=${core#*+}
    core=${core%%+*}
    ;;
esac
case "$core" in
  # A '-' inside the numeric core is impossible, so the first one starts the
  # pre-release.
  *-*)
    has_prerelease=true
    prerelease=${core#*-}
    core=${core%%-*}
    ;;
esac

case "$core" in
  v*) core=${core#v} ;;
  *) fail "version '$version' must start with 'v'" ;;
esac

# Exactly three dot-separated numeric fields, none with a leading zero.
IFS=. read -r major minor patch extra <<EOF
$core
EOF
[ -z "${extra:-}" ] || fail "version '$version' has more than three numeric fields"
for field_pair in "major:${major-}" "minor:${minor-}" "patch:${patch-}"; do
  name=${field_pair%%:*}
  value=${field_pair#*:}
  [ -n "$value" ] || fail "version '$version' has an empty $name field"
  case "$value" in
    *[!0-9]*) fail "version '$version' has a non-numeric $name field '$value'" ;;
    0) ;;
    0*) fail "version '$version' has a leading zero in its $name field '$value'" ;;
  esac
done

# Pre-release: dot-separated identifiers, each non-empty, alphanumerics and
# hyphens only, and no leading zero on a purely numeric one.
if [ "$has_prerelease" = true ]; then
  [ -n "$prerelease" ] || fail "version '$version' has a trailing '-' with no pre-release"
  remainder=$prerelease
  while :; do
    case "$remainder" in
      *.*)
        identifier=${remainder%%.*}
        remainder=${remainder#*.}
        ;;
      *)
        identifier=$remainder
        remainder=
        ;;
    esac
    [ -n "$identifier" ] || fail "version '$version' has an empty pre-release identifier"
    case "$identifier" in
      *[!0-9A-Za-z-]*) fail "version '$version' has an invalid character in pre-release identifier '$identifier'" ;;
    esac
    case "$identifier" in
      *[!0-9]*) ;;
      0) ;;
      0*) fail "version '$version' has a leading zero in numeric pre-release identifier '$identifier'" ;;
    esac
    [ -n "$remainder" ] || break
  done
fi

# Build metadata: same identifier rules, but leading zeroes are permitted since
# the identifiers are never compared numerically.
if [ "$has_build" = true ]; then
  [ -n "$build" ] || fail "version '$version' has a trailing '+' with no build metadata"
  remainder=$build
  while :; do
    case "$remainder" in
      *.*)
        identifier=${remainder%%.*}
        remainder=${remainder#*.}
        ;;
      *)
        identifier=$remainder
        remainder=
        ;;
    esac
    [ -n "$identifier" ] || fail "version '$version' has an empty build identifier"
    case "$identifier" in
      *[!0-9A-Za-z-]*) fail "version '$version' has an invalid character in build identifier '$identifier'" ;;
    esac
    [ -n "$remainder" ] || break
  done
fi

case "$prerelease" in
  incompatible|incompatible.*)
    fail "version '$version' uses the +incompatible resolver artifact, which must not be published as a tag"
    ;;
esac
case "$build" in
  incompatible|incompatible.*)
    fail "version '$version' uses the +incompatible resolver artifact, which must not be published as a tag"
    ;;
esac

# A v2+ tag requires a matching module path suffix. Publishing v2.0.0 from a
# module declared as v1 yields a version the proxy will serve but `go get` cannot
# resolve.
if [ -n "$module" ]; then
  case "$major" in
    0 | 1)
      case "$module" in
        */v[0-9] | */v[0-9][0-9])
          fail "module path '$module' carries a major-version suffix but '$version' is v$major"
          ;;
      esac
      ;;
    *)
      if [ "${module##*/}" != "v$major" ]; then
        fail "releasing '$version' requires the module path to end in /v$major, but it is '$module'"
      fi
      ;;
  esac
fi

echo "OK: $version is a canonical Go semantic version${module:+ releasable from $module}"
