#!/usr/bin/env bash
# Verify a complete release has the exact metadata and bytes selected for publication.
set -euo pipefail

if [[ $# -ne 6 ]]; then
  echo "usage: $0 RELEASE_JSON TAG COMMIT ASSET EXPECTED_SHA256 DOWNLOAD_DIR" >&2
  exit 2
fi

release_json=$1
tag=$2
commit=$3
asset=$4
expected=$5
download_dir=$6
checksum="$asset.sha256"

actual_tag=$(jq -r '.tagName // .tag_name // empty' "$release_json")
actual_target=$(jq -r '.targetCommitish // .target_commitish // empty' "$release_json")
actual_prerelease=$(jq -r '.isPrerelease // .prerelease // false' "$release_json")
if [[ "$actual_tag" != "$tag" || "$actual_target" != "$commit" || "$actual_prerelease" != true ]]; then
  printf 'release metadata differs: expected tag=%s target=%s prerelease=true; actual tag=%s target=%s prerelease=%s\n' "$tag" "$commit" "$actual_tag" "$actual_target" "$actual_prerelease" >&2
  exit 1
fi

if ! jq -e --arg asset "$asset" --arg checksum "$checksum" '
  ([.assets[]?.name] | sort) == ([$asset, $checksum] | sort)
' "$release_json" >/dev/null; then
  printf 'release assets differ: expected exactly %s and %s\n' "$asset" "$checksum" >&2
  exit 1
fi

[[ -f "$download_dir/$asset" ]] || { echo "release archive was not downloaded" >&2; exit 1; }
[[ -f "$download_dir/$checksum" ]] || { echo "release checksum was not downloaded" >&2; exit 1; }
actual=$(shasum -a 256 "$download_dir/$asset" | awk '{print $1}')
if [[ "$actual" != "$expected" ]]; then
  printf 'release archive digest differs: expected=%s actual=%s\n' "$expected" "$actual" >&2
  exit 1
fi
grep -Fxq "$expected  $asset" "$download_dir/$checksum" || { echo "release checksum does not authenticate the archive" >&2; exit 1; }
