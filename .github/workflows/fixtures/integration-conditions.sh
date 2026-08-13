#!/usr/bin/env bash
set -euo pipefail

# Focused local fixture for the event gates in integration.yml. Keep the
# scenarios here aligned with the job-level expressions in that workflow.

should_run_integration() {
  local event="$1" ref="$2" pr_repo="$3" pr_author="$4" labels="$5"

  if [[ "$event" == schedule && "$ref" == refs/heads/main ]]; then
    return 0
  fi

  if [[ "$event" == workflow_dispatch && "$ref" == refs/heads/main ]]; then
    return 0
  fi

  if [[ "$event" == pull_request && "$pr_repo" == GordonBeeming/shunt &&
    "$pr_author" == OWNER && " $labels " == *" safe-to-test "* ]]; then
    return 0
  fi

  return 1
}

should_run_extended() {
  local event="$1" ref="$2"
  [[ "$event" == schedule && "$ref" == refs/heads/main ]] ||
    [[ "$event" == workflow_dispatch && "$ref" == refs/heads/main ]]
}

assert_result() {
  local expected="$1" actual="$2" scenario="$3"
  if [[ "$actual" != "$expected" ]]; then
    printf 'FAIL: %s (expected %s, got %s)\n' "$scenario" "$expected" "$actual" >&2
    exit 1
  fi
  printf 'PASS: %s\n' "$scenario"
}

run_check() {
  local expected_integration="$1" expected_extended="$2" scenario="$3"; shift 3
  local actual=skip
  if should_run_integration "$@"; then
    actual=run
  fi
  assert_result "$expected_integration" "$actual" "$scenario (integration)"

  actual=skip
  if should_run_extended "$1" "$2"; then
    actual=run
  fi
  assert_result "$expected_extended" "$actual" "$scenario (extended)"
}

run_check run run 'schedule on default main' \
  schedule refs/heads/main '' '' ''
run_check run run 'manual dispatch on main' \
  workflow_dispatch refs/heads/main '' '' ''
run_check skip skip 'manual dispatch on a non-main branch' \
  workflow_dispatch refs/heads/feature '' '' ''
run_check run skip 'same-repository owner PR marked safe' \
  pull_request refs/pull/10/merge GordonBeeming/shunt OWNER safe-to-test
run_check skip skip 'fork PR marked safe' \
  pull_request refs/pull/11/merge someone/shunt OWNER safe-to-test
run_check skip skip 'same-repository owner PR without approval label' \
  pull_request refs/pull/12/merge GordonBeeming/shunt OWNER ''

printf 'All integration workflow event fixtures passed.\n'
