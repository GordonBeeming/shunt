#!/usr/bin/env bash
# Delete an overflow release once. Treat an absent release as already deleted.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 TAG" >&2
  exit 2
fi

tag=$1
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

if gh release delete "$tag" --yes --cleanup-tag; then
  exit 0
fi

response=$(mktemp)
trap 'rm -f "$response"' EXIT
if "$script_dir/retry.sh" 4 "$script_dir/read-release.sh" "$response" "repos/$repo/releases/tags/$tag"; then
  status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$response" | tail -n 1)
  if [[ "$status" == 404 ]]; then
    echo "release deletion request failed after $tag was already absent; resuming" >&2
    exit 0
  fi
fi
echo "release deletion failed and $tag still exists" >&2
exit 1
