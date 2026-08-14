#!/usr/bin/env bash
# Produce the sole, reproducible macOS archive accepted by the release scripts.
set -euo pipefail
if [[ $# -ne 2 ]]; then echo "usage: $0 VERSION OUTPUT_DIRECTORY" >&2; exit 2; fi
version=$1
out=$2
asset=shunt-nightly_darwin_arm64.tar.gz
[[ "$version" =~ ^0\.0\.0-nightly\.[0-9]+$ ]] || { echo "invalid nightly version: $version" >&2; exit 2; }
mkdir -p "$out"
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "-buildid= -X github.com/gordonbeeming/shunt/internal/config.Channel=nightly -X github.com/gordonbeeming/shunt/internal/config.BuildVersion=$version" -o "$out/shunt-nightly" .
file "$out/shunt-nightly" | grep -Eq 'Mach-O.*arm64'
version_output=$("$out/shunt-nightly" version)
printf '%s\n' "$version_output"
[[ "$version_output" == *"channel=nightly"* && "$version_output" == *"binary=shunt-nightly"* && "$version_output" == *"version=$version"* ]]
TZ=UTC touch -t 197001020000 "$out/shunt-nightly"
COPYFILE_DISABLE=1 tar --format ustar --uid 0 --gid 0 --uname root --gname root --no-xattrs -cf - -C "$out" shunt-nightly | gzip -n > "$out/$asset"
[[ $(tar -tzf "$out/$asset") == shunt-nightly ]]
check_dir=$(mktemp -d)
trap 'rm -rf "$check_dir"' EXIT
tar -xzf "$out/$asset" -C "$check_dir"
test -x "$check_dir/shunt-nightly"
(cd "$out" && shasum -a 256 "$asset" > "$asset.sha256" && shasum -a 256 -c "$asset.sha256")
