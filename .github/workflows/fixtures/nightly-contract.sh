#!/usr/bin/env bash
# Static release-contract checks. These protect the trust boundary that YAML
# syntax validation cannot express: hosted publication and consumer validation
# must preserve the release and tap security boundaries.
# shellcheck disable=SC2016 # GitHub expressions and workflow literals are intentional.
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../../.." && pwd)
workflow="$root/.github/workflows/nightly.yml"
workflows_dir="$root/.github/workflows"
consumer="$root/packaging/nightly/consumer.sh"
formula="$root/packaging/homebrew/shunt-nightly.rb.tmpl"

require() {
  local text="$1" explanation="$2"
  grep -Fq -- "$text" "$workflow" || {
    printf 'missing nightly workflow contract: %s\n' "$explanation" >&2
    exit 1
  }
}

require_file() {
  local file="$1" text="$2" explanation="$3"
  grep -Fqx -- "$text" "$file" || {
    printf 'missing nightly packaging contract: %s\n' "$explanation" >&2
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

require_candidate_consumer() {
  local text="$1" explanation="$2"
  awk '
    /^  candidate-consumer:/ { in_job=1; next }
    in_job && /^  [^[:space:]]/ { exit }
    in_job { print }
  ' "$workflow" | grep -Fq -- "$text" || {
    printf 'missing candidate-consumer workflow contract: %s\n' "$explanation" >&2
    exit 1
  }
}

release_job() {
  awk '
    /^  release:/ { in_job=1; next }
    in_job && /^  [^[:space:]]/ { exit }
    in_job { print }
  ' "$workflow"
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

consumer_line_of() {
  local text="$1" occurrence=${2:-1}
  rg -n -F -- "$text" "$consumer" | sed -n "${occurrence}p" | cut -d: -f1
}

require 'package:' 'read-only package job'
require 'release:' 'minimal release mutation job'
require 'candidate-consumer:' 'anonymous candidate consumer gate'
require 'publish-tap:' 'formula publication after release'
require 'consumer-health:' 'visible public consumer health job'
require 'retention:' 'post-consumer retention job'
require 'needs: [gate, package]' 'release consumes the verified package job'
require 'needs: [gate, release, candidate-consumer]' 'tap publication waits for the anonymous candidate consumer'
require 'needs: [gate, release, publish-tap]' 'consumer health follows formula publication'
require 'needs: [gate, consumer-health]' 'retention waits for hosted consumer health only'
require 'actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02' 'full-SHA artifact upload pin'
require 'actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093' 'full-SHA artifact download pin'
require 'packaging/nightly/publish-release.sh "$TAG" "$VERSION" "$GITHUB_SHA" "$RUNNER_TEMP/release-payload"' 'release state reconciler receives the packaged payload'
require 'packaging/nightly/retry.sh 4 packaging/nightly/clone-tap.sh https://github.com/GordonBeeming/homebrew-tap.git "$tap_dir"' 'retrying tap clone for release selection'
require 'packaging/nightly/retry.sh 4 packaging/nightly/read-releases.sh "$RUNNER_TEMP/releases.json"' 'retrying paginated release reads'
require 'packaging/nightly/delete-release.sh "$old_tag"' 'state-reconciling release deletion'
require 'ref: ${{ github.sha }}' 'exact-SHA checkout'
require 'contents: write' 'write permission for the two GitHub release mutations'
require 'packaging/nightly/consumer.sh candidate "$VERSION" "$TAG" "$SHA256"' 'anonymous pre-tap candidate consumer check'
require 'packaging/nightly/consumer.sh tap "$VERSION" "$TAG" "$SHA256" "$PREVIOUS_VERSION" "$PREVIOUS_TAG" "$PREVIOUS_SHA256"' 'named public tap consumer health check'
require 'previous_version: ${{ steps.select.outputs.previous_version }}' 'prior formula version gate output'
require 'previous_tag: ${{ steps.select.outputs.previous_tag }}' 'prior formula tag gate output'
require 'previous_sha256: ${{ steps.select.outputs.previous_sha256 }}' 'prior formula checksum gate output'
require 'go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.11' 'pinned actionlint CLI installation'
require 'bash packaging/homebrew/consumer-fixtures.sh' 'hosted consumer-contract fixture coverage'
require 'packaging/homebrew/audit.sh "$RUNNER_TEMP/shunt-nightly.rb"' 'packaged formula audit uses a temporary named tap'
require 'cp packaging/homebrew/audit.sh "$RUNNER_TEMP/release-payload/homebrew-audit.sh"' 'audit helper travels with the authenticated release payload'
require 'bash "$RUNNER_TEMP/release-payload/homebrew-audit.sh" "$RUNNER_TEMP/release-payload/shunt-nightly.rb"' 'release-authenticated formula audit uses the packaged temporary-tap helper'
require_file "$formula" '  depends_on "go@1.25"' 'nightly formula uses the canonical versioned Go dependency'
require_file "$consumer" 'homebrew_go_formula=go@1.25' 'nightly consumer validates the formula dependency itself installs'
require_file "$consumer" 'formula_name=shunt-nightly' 'nightly consumer uses the formula identifier'
require_file "$formula" 'class ShuntNightly < Formula' 'nightly formula class matches its package'

onboarding_tokens=(
  'GO_BIN="$(brew --prefix go@1.25)/bin/go"'
  'XCADDY_BIN="$("$GO_BIN" env GOPATH | cut -d: -f1)/bin"'
  'GOBIN="$XCADDY_BIN" "$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6'
  'export PATH="$(brew --prefix go@1.25)/bin:$XCADDY_BIN:$PATH"'
)
for surface in "$root/README.md" "$root/CLAUDE.md" "$root/cmd/skill.go" "$formula"; do
  for token in "${onboarding_tokens[@]}"; do
    grep -Fq -- "$token" "$surface" || {
      printf 'missing shared nightly onboarding token in %s: %s\n' "$surface" "$token" >&2
      exit 1
    }
  done
  if grep -Fq -- 'GOPATH_BIN' "$surface"; then
    printf 'ambient GOPATH bin variable remains in %s\n' "$surface" >&2
    exit 1
  fi
done
require '(cd "$payload" && printf '\''%s  %s\n'\'' "$SHA256" shunt-nightly_darwin_arm64.tar.gz | shasum -a 256 -c -)' 'artifact digest verification from the downloaded payload directory'
require_consumer_health 'PREVIOUS_VERSION: ${{ needs.gate.outputs.previous_version }}' 'prior formula version reaches consumer health'
require_consumer_health 'PREVIOUS_TAG: ${{ needs.gate.outputs.previous_tag }}' 'prior formula tag reaches consumer health'
require_consumer_health 'PREVIOUS_SHA256: ${{ needs.gate.outputs.previous_sha256 }}' 'prior formula checksum reaches consumer health'
require_consumer_health 'runs-on: macos-26' 'consumer health uses the hosted macOS 26 runner'
require_consumer_health 'packaging/nightly/consumer.sh tap "$VERSION" "$TAG" "$SHA256" "$PREVIOUS_VERSION" "$PREVIOUS_TAG" "$PREVIOUS_SHA256"' 'consumer health validates the public tap install and upgrade path'
require_candidate_consumer 'runs-on: macos-26' 'candidate validation uses the hosted arm64 macOS 26 runner'
require_candidate_consumer 'packaging/nightly/consumer.sh candidate "$VERSION" "$TAG" "$SHA256"' 'candidate validation installs and tests the local formula before publication'
candidate_go_line=$(consumer_line_of '    assert_supported_homebrew_go' 1)
candidate_install_line=$(consumer_line_of '    brew install "$staged_formula"' 2)
candidate_test_line=$(consumer_line_of '    brew test "$staged_formula"' 2)
tap_go_line=$(consumer_line_of '    assert_supported_homebrew_go' 2)
tap_success_line=$(consumer_line_of '  assert_installed_consumer' 2)
[[ -n "$candidate_go_line" && -n "$candidate_install_line" && -n "$candidate_test_line" && \
  "$candidate_install_line" -lt "$candidate_go_line" && "$candidate_go_line" -lt "$candidate_test_line" ]] || {
  echo 'candidate Homebrew Go gate must run after formula install and before candidate success checks' >&2
  exit 1
}
[[ -n "$tap_go_line" && -n "$tap_success_line" && "$tap_go_line" -lt "$tap_success_line" ]] || {
  echo 'named-tap Homebrew Go gate must run before consumer health success' >&2
  exit 1
}
release_job | grep -Fq 'GH_TOKEN: ${{ secrets.IMMUTABLE_RELEASES_READ_TOKEN }}' || {
  echo 'release job must use the dedicated immutable settings read token' >&2
  exit 1
}
release_job | grep -Fq 'gh api "repos/$GITHUB_REPOSITORY/immutable-releases"' || {
  echo 'release job must verify immutable releases before mutation' >&2
  exit 1
}
immutable_preflight_line=$(release_job | rg -n -F 'gh api "repos/$GITHUB_REPOSITORY/immutable-releases"' | head -n1 | cut -d: -f1)
release_checkout_line=$(release_job | rg -n -F 'uses: actions/checkout@' | head -n1 | cut -d: -f1)
[[ -n "$immutable_preflight_line" && -n "$release_checkout_line" && "$immutable_preflight_line" -lt "$release_checkout_line" ]] || {
  echo 'immutable-release preflight must run before release checkout' >&2
  exit 1
}

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

if rg -n -i --glob '*.yml' -e 'self-hosted' -e 'macos-27' -e 'apple-container' "$workflows_dir"; then
  echo 'workflows must not reintroduce self-hosted Apple-container runner labels' >&2
  exit 1
fi

if rg -n --glob '*.yml' '^  container-health:' "$workflows_dir"; then
  echo 'workflows must not reintroduce the nightly container-health job' >&2
  exit 1
fi

if rg -n 'retry\.sh [0-9]+ gh release delete|retry\.sh [0-9]+ gh release (create|upload|edit)' "$workflow"; then
  echo 'nightly workflow retries a mutation instead of reconciling its state' >&2
  exit 1
fi

if rg -n -F 'brew audit' "$workflow"; then
  echo 'nightly workflow must route every Homebrew audit through the named-tap helper' >&2
  exit 1
fi

# Release must happen before the formula becomes externally visible; consumer
# health is intentionally later so retention only follows public validation.
assert_before 'name: Publish immutable release' 'name: Sign and publish Homebrew formula' 'immutable release must precede tap publication'
assert_before 'name: Validate anonymous release-formula candidate' 'name: Sign and publish Homebrew formula' 'anonymous candidate validation must precede tap publication'
assert_before 'name: Sign and publish Homebrew formula' 'name: Post-publication Homebrew consumer health' 'tap publication must precede consumer health'
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

printf 'nightly workflow contract fixtures passed\n'
