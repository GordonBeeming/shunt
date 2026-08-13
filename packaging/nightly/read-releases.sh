#!/usr/bin/env bash
# Fetch every release page as one JSON array for selection and retention reads.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 OUTPUT_FILE" >&2
  exit 2
fi

output=$1
repo=${GITHUB_REPOSITORY:-GordonBeeming/shunt}
temporary="${output}.tmp.$$"
trap 'rm -f "$temporary"' EXIT

gh api --paginate --slurp "repos/$repo/releases?per_page=100" > "$temporary"
jq -e 'if type == "array" and (length == 0 or (.[0] | type) == "array") then [ .[] | .[] ] elif type == "array" then . else error("release response must be an array") end' "$temporary" > "$output"
