#!/usr/bin/env bash
# Print old nightly release tags, retaining the current tag and 29 newest others.
set -euo pipefail
if [[ $# -ne 2 ]]; then echo "usage: $0 CURRENT_TAG RELEASES_JSON" >&2; exit 2; fi
current=$1
jq -r --arg current "$current" '[.[] | select(.prerelease == true and (.tag_name | test("^nightly-[0-9]+$")) and .tag_name != $current)] | sort_by(.created_at) | reverse | .[29:][]?.tag_name' "$2"
