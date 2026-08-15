#!/usr/bin/env bash
# Find exactly one published or draft release by tag.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 OUTPUT_FILE TAG" >&2
  exit 2
fi

output=$1
tag=$2
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
mv "$temporary" "$output"
