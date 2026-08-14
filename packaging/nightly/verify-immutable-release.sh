#!/usr/bin/env bash
# GitHub returns immutable=true only after a published immutable release is locked.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 RELEASE_JSON" >&2
  exit 2
fi

release_json=$1
draft=$(jq -r '.draft // .isDraft // false' "$release_json")
immutable=$(jq -r '.immutable // false' "$release_json")
if [[ "$draft" != false || "$immutable" != true ]]; then
  printf 'release is not an immutable published release: draft=%s immutable=%s\n' "$draft" "$immutable" >&2
  exit 1
fi
