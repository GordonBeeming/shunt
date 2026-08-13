#!/usr/bin/env bash
# Clone a public tap into an empty destination. A failed clone never leaves state for retry.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 REPOSITORY DESTINATION" >&2
  exit 2
fi

repository=$1
destination=$2
[[ ! -e "$destination" ]] || { echo "clone destination already exists: $destination" >&2; exit 2; }

temporary="${destination}.tmp.$$"
trap 'rm -rf "$temporary"' EXIT
git clone --depth=1 "$repository" "$temporary"
mv "$temporary" "$destination"
