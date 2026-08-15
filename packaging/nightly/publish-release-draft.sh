#!/usr/bin/env bash
# Publish a complete draft once. Reconcile ambiguous success by reading its state.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 TAG RELEASE_ID COMMIT ASSET" >&2
  exit 2
fi

tag=$1
release_id=$2
commit=$3
asset=$4
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

[[ "$release_id" =~ ^[1-9][0-9]*$ ]] || { echo "release ID must be positive: $release_id" >&2; exit 2; }

if gh api "repos/$repo/releases/$release_id" --method PATCH -F draft=false >/dev/null; then
  exit 0
fi

response=$(mktemp)
trap 'rm -f "$response"' EXIT
if "$script_dir/retry-not-found.sh" 4 "$script_dir/find-published-release.sh" "$response" "$tag" "$commit" "$asset"; then
  echo "release publish request failed after $tag was published; resuming" >&2
  exit 0
fi
echo "release publish failed and $tag was not published with the expected state" >&2
exit 1
