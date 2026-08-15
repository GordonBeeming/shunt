#!/usr/bin/env bash
# Find exactly one published or draft release by tag.
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: $0 OUTPUT_FILE TAG [REQUIRED_ASSET]" >&2
  exit 2
fi

output=$1
tag=$2
required_asset=${3:-}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
temporary="${output}.tmp.$$"
releases="${output}.releases.$$"
trap 'rm -f "$temporary" "$releases"' EXIT

"$script_dir/retry.sh" 4 "$script_dir/read-releases.sh" "$releases"
count=$(jq --arg tag "$tag" '[.[] | select(.tag_name == $tag)] | length' "$releases")
case "$count" in
  0) exit 4 ;;
  1) jq --arg tag "$tag" '.[] | select(.tag_name == $tag)' "$releases" > "$temporary" ;;
  *) echo "multiple releases exist for tag $tag" >&2; exit 1 ;;
esac
if [[ -n "$required_asset" ]] && ! jq -e --arg name "$required_asset" \
  '[.assets[] | select(.name == $name)] | length == 1' "$temporary" >/dev/null; then
  exit 4
fi
mv "$temporary" "$output"
