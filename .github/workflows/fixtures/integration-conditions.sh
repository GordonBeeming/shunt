#!/usr/bin/env bash
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)
workflows_dir="$root/.github/workflows"

if [[ -e "$workflows_dir/integration.yml" ]]; then
  echo 'self-hosted integration workflow must remain removed' >&2
  exit 1
fi

if rg -n -i --glob '*.yml' -e 'self-hosted' -e 'macos-27' -e 'apple-container' "$workflows_dir"; then
  echo 'hosted-only workflows must not contain Apple-container runner labels' >&2
  exit 1
fi

printf 'Hosted-only workflow fixture passed. Local pre-push gate: SHUNT_CONTAINER_INTEGRATION=1 go test -p 1 -tags integration ./... -count=1 -timeout 30m\n'
