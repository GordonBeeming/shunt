#!/usr/bin/env bash
# Publish a complete draft once. Reconcile ambiguous success by reading its state.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 TAG COMMIT ASSET" >&2
  exit 2
fi

tag=$1
commit=$2
asset=$3
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

if gh release edit "$tag" --draft=false; then
  exit 0
fi

response=$(mktemp)
trap 'rm -f "$response" "$response.json"' EXIT
if "$script_dir/retry.sh" 4 "$script_dir/read-release.sh" "$response" "repos/$repo/releases/tags/$tag"; then
  status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$response" | tail -n 1)
  if [[ "$status" == 200 ]]; then
    sed '1,/^$/d' "$response" > "$response.json"
    if jq -e --arg tag "$tag" --arg commit "$commit" --arg asset "$asset" '(.tag_name == $tag) and (.target_commitish == $commit) and (.draft == false) and (.prerelease == true) and ([.assets[] | select(.name == $asset)] | length == 1)' "$response.json" >/dev/null; then
      echo "release publish request failed after $tag was published; resuming" >&2
      exit 0
    fi
  fi
fi
echo "release publish failed and $tag was not published with the expected state" >&2
exit 1
