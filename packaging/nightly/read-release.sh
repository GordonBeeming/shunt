#!/usr/bin/env bash
# Fetch a release response. A 404 is an expected state and is not retried.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 OUTPUT_FILE RELEASE_ENDPOINT" >&2
  exit 2
fi

output=$1
endpoint=$2
temporary="${output}.tmp.$$"
trap 'rm -f "$temporary"' EXIT

set +e
gh api -i "$endpoint" > "$temporary"
gh_status=$?
set -e
mv "$temporary" "$output"

http_status=$(sed -n 's/^HTTP\/[^ ]* \([0-9][0-9][0-9]\).*/\1/p' "$output" | tail -n 1)
case "$http_status" in
  200|404) exit 0 ;;
  *)
    printf 'release lookup failed: endpoint=%s http_status=%s gh_status=%s\n' "$endpoint" "${http_status:-unknown}" "$gh_status" >&2
    cat "$output" >&2
    exit 1
    ;;
esac
