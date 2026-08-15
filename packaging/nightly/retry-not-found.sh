#!/usr/bin/env bash
# Retry only lookup misses; permanent failures must remain immediately visible.
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 ATTEMPTS COMMAND..." >&2
  exit 2
fi

attempts=$1
shift
[[ "$attempts" =~ ^[1-9][0-9]*$ ]] || { echo "attempts must be positive: $attempts" >&2; exit 2; }

for attempt in $(seq 1 "$attempts"); do
  if "$@"; then
    exit 0
  else
    status=$?
  fi
  if [[ "$status" != 4 || "$attempt" == "$attempts" ]]; then
    exit "$status"
  fi

  base_delay=$((1 << (attempt - 1)))
  (( base_delay > 8 )) && base_delay=8
  jitter_ms=$((RANDOM % 501))
  printf -v delay '%s.%03d' "$base_delay" "$jitter_ms"
  printf 'lookup attempt %s/%s was not found; retrying in %ss:' "$attempt" "$attempts" "$delay" >&2
  for argument in "$@"; do printf ' %q' "$argument" >&2; done
  printf '\n' >&2
  sleep "$delay"
done
