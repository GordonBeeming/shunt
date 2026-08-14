#!/bin/sh
# Render the public Homebrew formula from the checked-in template. Inputs are
# constrained before substitution so the generated Ruby cannot be changed by a
# release-tag or checksum value.
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 VERSION TAG SHA256 OUTPUT" >&2
  exit 2
fi

version=$1
tag=$2
sha256=$3
output=$4

if ! printf '%s' "$version" | grep -Eq '^0\.0\.0-nightly\.[0-9]+$'; then
  echo "invalid nightly version: $version" >&2
  exit 2
fi

if ! printf '%s' "$tag" | grep -Eq '^nightly-[0-9]+$'; then
  echo "invalid nightly tag: $tag" >&2
  exit 2
fi

if [ "${#sha256}" -ne 64 ] || ! printf '%s' "$sha256" | grep -Eq '^[0-9a-f]{64}$'; then
  echo "invalid SHA-256: $sha256" >&2
  exit 2
fi

template_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
template="$template_dir/shunt-nightly.rb.tmpl"

if [ ! -f "$template" ]; then
  echo "formula template not found: $template" >&2
  exit 2
fi

mkdir -p "$(dirname -- "$output")"
sed \
  -e "s/__VERSION__/$version/g" \
  -e "s/__TAG__/$tag/g" \
  -e "s/__SHA256__/$sha256/g" \
  "$template" > "$output"

if grep -q '__\(VERSION\|TAG\|SHA256\)__' "$output"; then
  echo "formula rendering left an unresolved placeholder" >&2
  exit 1
fi
