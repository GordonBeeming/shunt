#!/usr/bin/env bash
# Prove every existing draft asset matches the candidate before any upload.
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 RELEASE_JSON ASSET EXPECTED_SHA256 DOWNLOAD_DIR" >&2
  exit 2
fi

release_json=$1
asset=$2
expected=$3
download_dir=$4
checksum="$asset.sha256"

[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid expected SHA-256" >&2; exit 2; }
[[ -d "$download_dir" ]] || { echo "download directory does not exist: $download_dir" >&2; exit 2; }

asset_count=$(jq --arg name "$asset" '[.assets[] | select(.name == $name)] | length' "$release_json")
checksum_count=$(jq --arg name "$checksum" '[.assets[] | select(.name == $name)] | length' "$release_json")
if [[ "$asset_count" -gt 1 || "$checksum_count" -gt 1 ]]; then
  printf 'release asset names must be unique: asset_count=%s checksum_count=%s\n' "$asset_count" "$checksum_count" >&2
  exit 1
fi

if [[ "$asset_count" == 1 ]]; then
  [[ -f "$download_dir/$asset" ]] || { echo "existing archive was not downloaded" >&2; exit 1; }
  actual=$(shasum -a 256 "$download_dir/$asset" | awk '{print $1}')
  [[ "$actual" == "$expected" ]] || { printf 'existing archive digest mismatch: expected=%s actual=%s\n' "$expected" "$actual" >&2; exit 1; }
fi
if [[ "$checksum_count" == 1 ]]; then
  [[ -f "$download_dir/$checksum" ]] || { echo "existing checksum was not downloaded" >&2; exit 1; }
  grep -Fxq "$expected  $asset" "$download_dir/$checksum" || { echo "existing checksum does not authenticate the candidate archive" >&2; exit 1; }
fi

echo "existing release bytes match the candidate archive"
