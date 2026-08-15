#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || ! -f "$1" ]]; then
  echo "usage: $0 <formula.rb>" >&2
  exit 2
fi

formula=$1
tap_name="shunt-audit-$RANDOM-$RANDOM/nightly"

cleanup() {
  brew untap --force "$tap_name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

brew tap-new --no-git "$tap_name" >/dev/null
tap_dir=$(brew --repo "$tap_name")
cp "$formula" "$tap_dir/Formula/shunt-nightly.rb"
brew audit --strict --formula "$tap_name/shunt-nightly"
