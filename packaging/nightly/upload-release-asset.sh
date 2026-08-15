#!/usr/bin/env bash
# Upload one draft asset once. Reconcile ambiguous success by checking its name.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 TAG RELEASE_ID ASSET_FILE" >&2
  exit 2
fi

tag=$1
release_id=$2
asset_file=$3
asset_name=$(basename -- "$asset_file")
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
[[ -f "$asset_file" ]] || { echo "asset file does not exist: $asset_file" >&2; exit 2; }
[[ "$release_id" =~ ^[1-9][0-9]*$ ]] || { echo "release ID must be positive: $release_id" >&2; exit 2; }

if gh api "https://uploads.github.com/repos/$repo/releases/$release_id/assets?name=$asset_name" \
  --method POST \
  -H 'Content-Type: application/octet-stream' \
  --input "$asset_file" >/dev/null; then
  exit 0
fi

response=$(mktemp)
trap 'rm -f "$response" "$response.json"' EXIT
if "$script_dir/retry.sh" 4 "$script_dir/read-release.sh" "$response" "repos/$repo/releases/$release_id"; then
  status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$response" | tail -n 1)
  if [[ "$status" == 200 ]]; then
    sed '1,/^$/d' "$response" > "$response.json"
    if jq -e --arg name "$asset_name" '[.assets[] | select(.name == $name)] | length == 1' "$response.json" >/dev/null; then
      echo "asset upload request failed after $asset_name was created; resuming" >&2
      exit 0
    fi
  fi
fi
echo "asset upload failed and the asset was not found on $tag" >&2
exit 1
