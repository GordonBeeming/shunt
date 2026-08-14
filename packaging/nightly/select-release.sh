#!/usr/bin/env bash
# Select a nightly tag and preserve the current public formula for consumer health.
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 FORCE RUN_NUMBER RELEASES_JSON" >&2
  exit 2
fi

force=$1
run_number=$2
releases_json=$3
commit=${GITHUB_SHA:?GITHUB_SHA is required}
formula=${TAP_FORMULA:-}
[[ "$force" == true || "$force" == false ]] || { echo "force must be true or false" >&2; exit 2; }
[[ "$run_number" =~ ^[0-9]+$ ]] || { echo "run number must be numeric" >&2; exit 2; }

previous_version=""
previous_tag=""
previous_sha256=""
if [[ -n "$formula" && -f "$formula" ]]; then
  previous_version=$(sed -nE 's/^[[:space:]]*version "([^"]+)"[[:space:]]*$/\1/p' "$formula" | head -n 1)
  previous_tag=$(sed -n 's#.*releases/download/\([^/]*\)/.*#\1#p' "$formula" | head -n 1)
  previous_sha256=$(sed -nE 's/^[[:space:]]*sha256 "([0-9a-f]{64})"[[:space:]]*$/\1/p' "$formula" | head -n 1)

  [[ "$previous_version" =~ ^0\.0\.0-nightly\.[0-9]+$ && "$previous_tag" =~ ^nightly-[0-9]+$ && "$previous_sha256" =~ ^[0-9a-f]{64}$ ]] || {
    echo "published tap formula does not contain valid nightly release metadata" >&2
    exit 1
  }
  [[ "$previous_version" == "0.0.0-nightly.${previous_tag#nightly-}" ]] || {
    echo "published tap formula version and release tag disagree" >&2
    exit 1
  }
fi

matching_tags=$(jq -r --arg commit "$commit" '.[] | select(.prerelease == true and .target_commitish == $commit and (.tag_name | test("^nightly-[0-9]+$"))) | .tag_name' "$releases_json" | sort -t- -k2,2n)
if [[ "$force" == true ]]; then
  tag="nightly-$run_number"
elif [[ -n "$previous_tag" ]] && printf '%s\n' "$matching_tags" | grep -Fxq "$previous_tag"; then
  tag=$previous_tag
elif [[ -n "$matching_tags" ]]; then
  tag=$(printf '%s\n' "$matching_tags" | tail -n 1)
else
  tag="nightly-$run_number"
fi

version="0.0.0-nightly.${tag#nightly-}"
printf 'tag=%s\nversion=%s\nprevious_version=%s\nprevious_tag=%s\nprevious_sha256=%s\n' \
  "$tag" "$version" "$previous_version" "$previous_tag" "$previous_sha256"
