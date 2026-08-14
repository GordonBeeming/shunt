#!/usr/bin/env bash
# Create a draft once. On an ambiguous failure, accept only the exact remote draft.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 TAG VERSION COMMIT NOTES_FILE" >&2
  exit 2
fi

tag=$1
version=$2
commit=$3
notes_file=$4
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

[[ -f "$notes_file" ]] || { echo "notes file does not exist: $notes_file" >&2; exit 2; }

if gh release create "$tag" --target "$commit" --title "Shunt nightly $version" --notes-file "$notes_file" --draft --prerelease --latest=false; then
  exit 0
fi

response=$(mktemp)
trap 'rm -f "$response"' EXIT
if ! "$script_dir/retry.sh" 4 "$script_dir/read-release.sh" "$response" "repos/$repo/releases/tags/$tag"; then
  echo "draft creation failed and the release could not be reconciled" >&2
  exit 1
fi
status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$response" | tail -n 1)
[[ "$status" == 200 ]] || { echo "draft creation failed and no release exists for $tag" >&2; exit 1; }
sed '1,/^$/d' "$response" > "$response.json"
trap 'rm -f "$response" "$response.json"' EXIT
if jq -e --arg tag "$tag" --arg commit "$commit" '(.tag_name == $tag) and (.target_commitish == $commit) and (.prerelease == true)' "$response.json" >/dev/null; then
  echo "draft create request failed after the matching release was created; resuming" >&2
  exit 0
fi
echo "draft creation failed and the existing release does not match the requested draft" >&2
exit 1
