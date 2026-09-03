#!/usr/bin/env bash
# Resolve a tag on a remote to the commit it names, printing the commit SHA.
#
# Usage: resolve-remote-tag.sh <remote> <tag>
#
# Exits 0 and prints the SHA when the tag exists, 1 with no output when it does
# not, and 2 on an unexpected reply. The distinction matters at the release gate:
# "no such tag" is the normal first-release path, while "cannot tell" must never be
# treated as "safe to create".
#
# Both tag kinds resolve without the caller knowing which is in play. `git
# ls-remote` reports an annotated tag on two lines — the tag object, and a peeled
# `refs/tags/<tag>^{}` line naming the commit — while a lightweight tag has only
# the first line, and there it is already the commit. Reading the peeled line when
# present and the plain line otherwise covers both.
set -uo pipefail

remote=${1-}
tag=${2-}

if [ -z "$remote" ] || [ -z "$tag" ]; then
  echo "usage: resolve-remote-tag.sh <remote> <tag>" >&2
  exit 2
fi

refs=$(git ls-remote --tags "$remote" "refs/tags/${tag}" "refs/tags/${tag}^{}" 2>/dev/null)
if [ -z "$refs" ]; then
  exit 1
fi

peeled=$(printf '%s\n' "$refs" | awk '$2 == "refs/tags/'"$tag"'^{}" {print $1; exit}')
if [ -n "$peeled" ]; then
  printf '%s\n' "$peeled"
  exit 0
fi

plain=$(printf '%s\n' "$refs" | awk '$2 == "refs/tags/'"$tag"'" {print $1; exit}')
if [ -n "$plain" ]; then
  printf '%s\n' "$plain"
  exit 0
fi

echo "::error::could not resolve refs/tags/${tag} on ${remote} from: ${refs}" >&2
exit 2
