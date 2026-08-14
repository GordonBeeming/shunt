#!/usr/bin/env bash
# Reconcile one nightly tag through draft, verified assets, and immutable publication.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 TAG VERSION COMMIT PAYLOAD_DIR" >&2
  exit 2
fi

tag=$1
version=$2
commit=$3
payload_dir=$4
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
asset=shunt-nightly_darwin_arm64.tar.gz
checksum="$asset.sha256"
archive_path="$payload_dir/$asset"
checksum_path="$payload_dir/$checksum"

[[ "$tag" =~ ^nightly-[0-9]+$ ]] || { echo "tag must be nightly-N: $tag" >&2; exit 2; }
[[ "$version" == "0.0.0-nightly.${tag#nightly-}" ]] || { echo "version does not match tag: version=$version tag=$tag" >&2; exit 2; }
[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || { echo "commit must be a full lowercase SHA" >&2; exit 2; }
[[ -f "$archive_path" && -f "$checksum_path" ]] || { echo "payload directory must contain $asset and $checksum" >&2; exit 2; }

expected=$(shasum -a 256 "$archive_path" | awk '{print $1}')
grep -Fxq "$expected  $asset" "$checksum_path" || { echo "payload checksum does not authenticate $asset" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
release_response="$work/release.response"
release_json="$work/release.json"
download_dir="$work/downloads"
mkdir -p "$download_dir"

fetch_release() {
  "$script_dir/retry.sh" 4 "$script_dir/read-release.sh" "$release_response" "repos/$repo/releases/tags/$tag"
  local status
  status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$release_response" | tail -n 1)
  if [[ "$status" == 404 ]]; then
    return 4
  fi
  [[ "$status" == 200 ]] || { echo "unexpected release lookup state: $status" >&2; return 1; }
  sed '1,/^$/d' "$release_response" > "$release_json"
}

assert_metadata() {
  jq -e --arg tag "$tag" --arg commit "$commit" '(.tag_name == $tag) and (.target_commitish == $commit) and (.prerelease == true)' "$release_json" >/dev/null || {
    echo "existing release does not match the requested tag, commit, and prerelease state" >&2
    return 1
  }
}

download_existing() {
  local names=()
  jq -e --arg name "$asset" '.assets[] | select(.name == $name)' "$release_json" >/dev/null && names+=(--pattern "$asset")
  jq -e --arg name "$checksum" '.assets[] | select(.name == $name)' "$release_json" >/dev/null && names+=(--pattern "$checksum")
  ((${#names[@]} == 0)) || "$script_dir/retry.sh" 4 gh release download "$tag" --dir "$download_dir" --clobber "${names[@]}"
}

require_immutable_or_cleanup() {
  if "$script_dir/verify-immutable-release.sh" "$release_json"; then
    return 0
  fi

  # The workflow token cannot read the admin-only immutable-release setting.
  # This authoritative response is the lock boundary. The exact candidate was
  # authenticated before this call, so it is safe to reconcile its deletion
  # before failing; no downstream candidate or tap step may continue.
  echo "published release $tag was mutable; reconciling deletion before aborting" >&2
  if "$script_dir/delete-release.sh" "$tag"; then
    echo "deleted mutable release $tag; enable immutable releases before retrying" >&2
  else
    echo "could not reconcile deletion of mutable release $tag" >&2
  fi
  return 1
}

if fetch_release; then
  assert_metadata
else
  fetch_status=$?
  if [[ "$fetch_status" != 4 ]]; then
    exit "$fetch_status"
  fi
  notes="$work/notes.md"
  printf 'Commit: %s\nRun: %s\n' "$commit" "${GITHUB_RUN_NUMBER:-unknown}" > "$notes"
  "$script_dir/create-release-draft.sh" "$tag" "$version" "$commit" "$notes"
  fetch_release
  assert_metadata
fi

is_draft=$(jq -r '.draft // false' "$release_json")
if [[ "$is_draft" == false ]]; then
  download_existing
  "$script_dir/verify-release.sh" "$release_json" "$tag" "$commit" "$asset" "$expected" "$download_dir"
  require_immutable_or_cleanup || exit 1
  echo "published immutable release already exactly matches $tag"
  exit 0
fi
[[ "$is_draft" == true ]] || { echo "release draft state is not boolean" >&2; exit 1; }

download_existing
"$script_dir/validate-partial-release.sh" "$release_json" "$asset" "$expected" "$download_dir"
for path in "$archive_path" "$checksum_path"; do
  name=$(basename -- "$path")
  if ! jq -e --arg name "$name" '.assets[] | select(.name == $name)' "$release_json" >/dev/null; then
    "$script_dir/upload-release-asset.sh" "$tag" "$path"
    fetch_release
    assert_metadata
    [[ $(jq -r '.draft // false' "$release_json") == true ]] || { echo "release was published while assets were being uploaded" >&2; exit 1; }
    # Re-authenticate after each remote mutation. A race cannot turn a valid
    # partial release into an accepted mismatched release.
    rm -f "$download_dir/$asset" "$download_dir/$checksum"
    download_existing
    "$script_dir/validate-partial-release.sh" "$release_json" "$asset" "$expected" "$download_dir"
  fi
done

"$script_dir/verify-release.sh" "$release_json" "$tag" "$commit" "$asset" "$expected" "$download_dir"
"$script_dir/publish-release-draft.sh" "$tag" "$commit" "$asset"
fetch_release
# The files fetched while this release was a draft are no longer trustworthy at
# the immutable lock boundary. Fetch both assets again from the now-published
# release before accepting the locked state.
rm -f "$download_dir/$asset" "$download_dir/$checksum"
download_existing
"$script_dir/verify-release.sh" "$release_json" "$tag" "$commit" "$asset" "$expected" "$download_dir"
require_immutable_or_cleanup || exit 1
echo "published immutable nightly release $tag"
