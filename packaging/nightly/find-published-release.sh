#!/usr/bin/env bash
# Find one release and verify that it has the complete published state.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 OUTPUT_FILE TAG COMMIT ASSET" >&2
  exit 2
fi

output=$1
tag=$2
commit=$3
asset=$4
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
temporary="${output}.candidate.$$"
trap 'rm -f "$temporary"' EXIT

if "$script_dir/find-release.sh" "$temporary" "$tag"; then
  :
else
  exit $?
fi
jq -e --arg tag "$tag" --arg commit "$commit" --arg asset "$asset" '
  (.tag_name == $tag) and (.target_commitish == $commit) and
  (.draft == false) and (.prerelease == true) and
  ([.assets[] | select(.name == $asset)] | length == 1)
' "$temporary" >/dev/null || exit 4
mv "$temporary" "$output"
