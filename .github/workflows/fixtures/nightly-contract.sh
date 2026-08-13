#!/usr/bin/env bash
# Static release-contract checks. These protect the trust boundary that YAML
# syntax validation cannot express: hosted publication must not wait for the
# optional macOS-27 health capacity, and credentialed code stays tiny.
# shellcheck disable=SC2016 # GitHub expressions and workflow literals are intentional.
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)
workflow="$root/.github/workflows/nightly.yml"
integration="$root/.github/workflows/integration.yml"

require() {
  local text="$1" explanation="$2"
  grep -Fq -- "$text" "$workflow" || {
    printf 'missing nightly workflow contract: %s\n' "$explanation" >&2
    exit 1
  }
}

require_consumer_health() {
  local text="$1" explanation="$2"
  awk '
    /^  consumer-health:/ { in_job=1; next }
    in_job && /^  [^[:space:]]/ { exit }
    in_job { print }
  ' "$workflow" | grep -Fq -- "$text" || {
    printf 'missing consumer-health workflow contract: %s\n' "$explanation" >&2
    exit 1
  }
}

line_of() {
  local text="$1"
  rg -n -F -- "$text" "$workflow" | head -n1 | cut -d: -f1
}

assert_before() {
  local first="$1" second="$2" explanation="$3"
  local first_line second_line
  first_line=$(line_of "$first")
  second_line=$(line_of "$second")
  [[ -n "$first_line" && -n "$second_line" && "$first_line" -lt "$second_line" ]] || {
    printf 'nightly workflow ordering violation: %s\n' "$explanation" >&2
    exit 1
  }
}

require 'package:' 'read-only package job'
require 'release:' 'minimal release mutation job'
require 'candidate-consumer:' 'anonymous candidate consumer gate'
require 'publish-tap:' 'formula publication after release'
require 'container-health:' 'visible Apple-container health job'
require 'consumer-health:' 'visible public consumer health job'
require 'retention:' 'post-consumer retention job'
require 'needs: [gate, package]' 'release consumes the verified package job'
require 'needs: [gate, release, candidate-consumer]' 'tap publication waits for the anonymous candidate consumer'
require 'needs: [gate, release]' 'container health follows publication'
require 'needs: [gate, release, publish-tap]' 'consumer health follows formula publication'
require 'needs: [gate, container-health, consumer-health]' 'retention waits for both post-publication health jobs'
require 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' 'full-SHA artifact upload pin'
require 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' 'full-SHA artifact download pin'
require 'packaging/nightly/publish-release.sh "$TAG" "$VERSION" "$GITHUB_SHA" "$RUNNER_TEMP/release-payload"' 'release state reconciler receives the packaged payload'
require 'packaging/nightly/retry.sh 4 packaging/nightly/clone-tap.sh https://github.com/GordonBeeming/homebrew-tap.git "$tap_dir"' 'retrying tap clone for release selection'
require 'packaging/nightly/retry.sh 4 packaging/nightly/read-releases.sh "$RUNNER_TEMP/releases.json"' 'retrying paginated release reads'
require 'packaging/nightly/delete-release.sh "$old_tag"' 'state-reconciling release deletion'
require 'SHUNT_CONTAINER_INTEGRATION: '\''1'\''' 'exact runtime activation environment'
require 'ref: ${{ github.sha }}' 'exact-SHA checkout'
require 'contents: write' 'write permission for the two GitHub release mutations'
require 'packaging/nightly/consumer.sh candidate "$VERSION" "$TAG" "$SHA256"' 'anonymous pre-tap candidate consumer check'
require 'packaging/nightly/consumer.sh tap "$VERSION" "$TAG" "$SHA256" "$PREVIOUS_VERSION" "$PREVIOUS_TAG" "$PREVIOUS_SHA256"' 'named public tap consumer health check'
require 'previous_version: ${{ steps.select.outputs.previous_version }}' 'prior formula version gate output'
require 'previous_tag: ${{ steps.select.outputs.previous_tag }}' 'prior formula tag gate output'
require 'previous_sha256: ${{ steps.select.outputs.previous_sha256 }}' 'prior formula checksum gate output'
require 'go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.11' 'pinned actionlint CLI installation'
require 'bash packaging/homebrew/consumer-fixtures.sh' 'hosted consumer-contract fixture coverage'
require '(cd "$payload" && printf '\''%s  %s\n'\'' "$SHA256" shunt-nightly_darwin_arm64.tar.gz | shasum -a 256 -c -)' 'artifact digest verification from the downloaded payload directory'
require_consumer_health 'PREVIOUS_VERSION: ${{ needs.gate.outputs.previous_version }}' 'prior formula version reaches consumer health'
require_consumer_health 'PREVIOUS_TAG: ${{ needs.gate.outputs.previous_tag }}' 'prior formula tag reaches consumer health'
require_consumer_health 'PREVIOUS_SHA256: ${{ needs.gate.outputs.previous_sha256 }}' 'prior formula checksum reaches consumer health'

write_scopes=$(rg -F -c 'contents: write' "$workflow" | awk -F: '{sum += $NF} END {print sum + 0}')
[[ "$write_scopes" == 2 ]] || {
  printf 'expected exactly two write-scoped nightly jobs, found %s\n' "$write_scopes" >&2
  exit 1
}

if rg -n 'continue-on-error' "$workflow"; then
  echo 'nightly workflow suppresses a health failure' >&2
  exit 1
fi

if rg -n -F --glob '*.yml' 'uses: rhysd/actionlint@' "$root/.github/workflows"; then
  echo 'workflows must install the pinned actionlint CLI instead of using the repository action' >&2
  exit 1
fi

if rg -n 'retry\.sh [0-9]+ gh release delete|retry\.sh [0-9]+ gh release (create|upload|edit)' "$workflow"; then
  echo 'nightly workflow retries a mutation instead of reconciling its state' >&2
  exit 1
fi

# Release must happen before the formula becomes externally visible; health is
# intentionally later so a missing self-hosted runner leaves visible queued
# work without preventing hosted publication.
assert_before 'name: Publish immutable release' 'name: Sign and publish Homebrew formula' 'immutable release must precede tap publication'
assert_before 'name: Validate anonymous release-formula candidate' 'name: Sign and publish Homebrew formula' 'anonymous candidate validation must precede tap publication'
assert_before 'name: Sign and publish Homebrew formula' 'name: Post-publication Homebrew consumer health' 'tap publication must precede consumer health'
assert_before 'name: Post-publication Apple container health' 'name: Retain successful public nightlies' 'container health must precede retention'
assert_before 'name: Post-publication Homebrew consumer health' 'name: Retain successful public nightlies' 'consumer health must precede retention'

secret_step=$(awk '
  /- name: Sign and push release-authenticated Homebrew formula/ { in_step=1; next }
  in_step && /^      - name:/ { exit }
  in_step { print }
' "$workflow")
if grep -Eq 'packaging/|\.sh($| )' <<<"$secret_step"; then
  echo 'secret-bearing formula step invokes checked-in helper code' >&2
  exit 1
fi

# Both independently triggerable integration jobs are main-only, and the PR
# gate is constrained to owner code that an owner explicitly approved.
for expected in \
  "github.event_name == 'schedule' && github.ref == 'refs/heads/main'" \
  "github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/main'" \
  "github.event.pull_request.head.repo.full_name == github.repository" \
  "github.event.pull_request.author_association == 'OWNER'" \
  "safe-to-test" \
  "SHUNT_CONTAINER_INTEGRATION: '1'"; do
  grep -Fq -- "$expected" "$integration" || {
    printf 'missing integration workflow fence: %s\n' "$expected" >&2
    exit 1
  }
done

printf 'nightly workflow contract fixtures passed\n'
