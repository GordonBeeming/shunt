#!/usr/bin/env bash
# Download one release asset by database ID, including from a draft release.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 ASSET_ID OUTPUT_FILE" >&2
  exit 2
fi

asset_id=$1
output=$2
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
[[ "$asset_id" =~ ^[1-9][0-9]*$ ]] || { echo "asset ID must be positive: $asset_id" >&2; exit 2; }
temporary="${output}.tmp.$$"
trap 'rm -f "$temporary"' EXIT

gh api "repos/$repo/releases/assets/$asset_id" \
  -H 'Accept: application/octet-stream' > "$temporary"
mv "$temporary" "$output"
