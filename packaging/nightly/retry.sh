#!/usr/bin/env bash
# Retry read-only operations with capped exponential backoff and bounded jitter.
set -euo pipefail

if [[ $# -lt 2 ]]; then
  echo "usage: $0 ATTEMPTS COMMAND..." >&2
  exit 2
fi

attempts=$1
shift
[[ "$attempts" =~ ^[1-9][0-9]*$ ]] || { echo "attempts must be positive" >&2; exit 2; }

for attempt in $(seq 1 "$attempts"); do
  if "$@"; then
    exit 0
  fi

  if [[ "$attempt" == "$attempts" ]]; then
    printf 'read failed after %s attempts: %q' "$attempt" "$1" >&2
    for argument in "$@"; do printf ' %q' "$argument" >&2; done
    printf '\n' >&2
    exit 1
  fi

  # Cap the base delay at eight seconds. Jitter keeps a concurrent rerun from
  # repeatedly colliding with the same API recovery window.
  base_delay=$((1 << (attempt - 1)))
  (( base_delay > 8 )) && base_delay=8
  jitter_ms=$((RANDOM % 501))
  printf -v delay '%s.%03d' "$base_delay" "$jitter_ms"
  printf 'read attempt %s/%s failed; retrying in %ss: %q' "$attempt" "$attempts" "$delay" "$1" >&2
  for argument in "$@"; do printf ' %q' "$argument" >&2; done
  printf '\n' >&2
  sleep "$delay"
done
