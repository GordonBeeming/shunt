#!/usr/bin/env bash
# Hermetic coverage for consumer first-install, upgrade, and anonymous probes.
# shellcheck disable=SC2016 # Literal onboarding tokens intentionally contain shell expansions.
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
version=0.0.0-nightly.42
previous=0.0.0-nightly.41
tag=nightly-42
sha256=$(printf payload | shasum -a 256 | awk '{print $1}')
previous_tag=nightly-41
previous_sha256=$(printf previous-payload | shasum -a 256 | awk '{print $1}')

grep -Fqx 'class ShuntNightly < Formula' "$root/packaging/homebrew/shunt-nightly.rb.tmpl"
grep -Fqx '  depends_on "go@1.25"' "$root/packaging/homebrew/shunt-nightly.rb.tmpl"
grep -Fqx 'formula_name=shunt-nightly' "$root/packaging/nightly/consumer.sh"
grep -Fqx 'homebrew_go_formula=go@1.25' "$root/packaging/nightly/consumer.sh"

nightly_onboarding_tokens=(
  'GO_BIN="$(brew --prefix go@1.25)/bin/go"'
  'XCADDY_BIN="$("$GO_BIN" env GOPATH | cut -d: -f1)/bin"'
  'GOBIN="$XCADDY_BIN" "$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6'
  'export PATH="$(brew --prefix go@1.25)/bin:$XCADDY_BIN:$PATH"'
)
for surface in \
  "$root/README.md" \
  "$root/CLAUDE.md" \
  "$root/cmd/skill.go" \
  "$root/packaging/homebrew/shunt-nightly.rb.tmpl"; do
  for token in "${nightly_onboarding_tokens[@]}"; do
    grep -Fq -- "$token" "$surface" || {
      echo "$surface omits nightly onboarding token: $token" >&2
      exit 1
    }
  done
  if grep -Fq -- 'GOPATH_BIN' "$surface"; then
    echo "$surface retains the ambient GOPATH bin variable" >&2
    exit 1
  fi
done

expect_failure() {
  if "$@"; then
    echo "fixture unexpectedly succeeded: $*" >&2
    exit 1
  fi
}

make_mock_tools() {
  local name=$1 installed=${2:-}
  local state="$tmp/$name"
  mkdir -p "$state/bin" "$state/cellar/bin" "$state/go/bin" "$state/tap/Formula"
  printf '%s' "$installed" > "$state/version"
  : > "$state/calls"
  cat > "$state/go/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
[[ "${GOENV:-}" == off && "${GOTOOLCHAIN:-}" == local ]] || {
  echo 'Homebrew Go fixture did not receive the deterministic environment' >&2
  exit 1
}
[[ "${1:-}" == version ]] || exit 2
echo "go $*" >> "${MOCK_BREW_STATE:?}/calls"
echo "${MOCK_GO_VERSION:?}"
GO
  chmod +x "$state/go/bin/go"
  cat > "$state/bin/go" <<'GO'
#!/usr/bin/env bash
echo 'go version go1.27.0 darwin/arm64'
GO
  chmod +x "$state/bin/go"
  cat > "$state/bin/brew" <<'BREW'
#!/usr/bin/env bash
set -euo pipefail
state=${MOCK_BREW_STATE:?}
target=${MOCK_TARGET_VERSION:?}
echo "brew $*" >> "$state/calls"
install_binary() {
  local installed_version=$1
  printf '%s' "$installed_version" > "$state/version"
  cat > "$state/cellar/bin/shunt-nightly" <<'BIN'
#!/usr/bin/env bash
set -euo pipefail
echo "shunt-nightly $*" >> "${MOCK_BREW_STATE:?}/calls"
case "${1:-}" in
  version) echo "shunt channel=nightly binary=shunt-nightly version=${MOCK_TARGET_VERSION:?}" ;;
  skill)
    mkdir -p "$HOME/.claude/skills/shunt" "$HOME/.codex/skills/shunt" "$HOME/.config/opencode/skills/shunt"
    printf 'shunt-nightly init\n.shunt-dev\n' > "$HOME/.codex/skills/shunt/SKILL.md"
    ;;
  *) exit 2 ;;
esac
BIN
  chmod +x "$state/cellar/bin/shunt-nightly"
  ln -sfn "$state/cellar/bin/shunt-nightly" "$state/bin/shunt-nightly"
}
case "${1:-}" in
  --prefix)
    if [[ "${2:-}" == go@1.25 ]]; then echo "$state/go"
    elif [[ $# -eq 1 ]]; then echo "$state"
    else echo "$state/cellar"
    fi
    ;;
  --repository)
    [[ "${2:-}" == shunt/nightly-candidate ]] || exit 2
    echo "$state/tap"
    ;;
  list)
    [[ "${2:-}" == --versions ]] || exit 2
    installed=$(cat "$state/version")
    [[ -z "$installed" ]] || echo "shunt-nightly $installed"
    ;;
  install)
    installed_version=$target
    if [[ "${2:-}" == *.rb && -f "${2:-}" ]]; then
      installed_version=$(sed -nE 's/^[[:space:]]*version "([^"]+)"[[:space:]]*$/\1/p' "$2" | head -n1)
    elif [[ "${2:-}" == shunt/nightly-candidate/shunt-nightly ]]; then
      installed_version=$(sed -nE 's/^[[:space:]]*version "([^"]+)"[[:space:]]*$/\1/p' "$state/tap/Formula/shunt-nightly.rb" | head -n1)
    fi
    install_binary "$installed_version"
    ;;
  upgrade) install_binary "$target" ;;
  uninstall) : > "$state/version"; rm -f "$state/bin/shunt-nightly" ;;
  test)
    [[ "${MOCK_BREW_TEST_FAIL:-false}" != true ]]
    ;;
  update) : ;;
  untap)
    [[ "${2:-}" == --force ]] || exit 2
    case "${3:-}" in
      gordonbeeming/tap) : ;;
      shunt/nightly-candidate) rm -f "$state/staging-tapped" ;;
      *) exit 2 ;;
    esac
    ;;
  tap-new)
    [[ "${2:-}" == --no-git && "${3:-}" == shunt/nightly-candidate ]] || exit 2
    : > "$state/staging-tapped"
    ;;
  tap)
    if [[ $# -eq 1 ]]; then
      printf '%s\n' gordonbeeming/tap
      [[ ! -e "$state/staging-tapped" ]] || printf '%s\n' shunt/nightly-candidate
    fi
    ;;
  *) echo "unexpected mock brew call: $*" >&2; exit 2 ;;
esac
BREW
  cat > "$state/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
state=${MOCK_BREW_STATE:?}
echo "curl $*" >> "$state/calls"
[[ "${MOCK_CURL_FAIL:-false}" != true ]] || exit 22
[[ "$*" == *"https://github.com/GordonBeeming/shunt/releases/download/nightly-42/shunt-nightly_darwin_arm64.tar.gz"* ]]
for ((index = 1; index <= $#; index++)); do
  if [[ "${!index}" == --output ]]; then
    next=$((index + 1))
    printf '%s' "${MOCK_CURL_PAYLOAD:-payload}" > "${!next}"
    break
  fi
done
CURL
  chmod +x "$state/bin/brew" "$state/bin/curl"
  if [[ -n "$installed" ]]; then
    MOCK_BREW_STATE="$state" MOCK_TARGET_VERSION="$installed" "$state/bin/brew" install fixture-seed
    : > "$state/calls"
  fi
  printf '%s' "$state"
}

run_consumer() {
  local state=$1
  shift
  PATH="$state/bin:$PATH" MOCK_BREW_STATE="$state" MOCK_TARGET_VERSION="$version" \
    MOCK_GO_VERSION="${MOCK_GO_VERSION:-go version go1.25.13 darwin/arm64}" SHUNT_NIGHTLY_CURL=curl \
    "$root/packaging/nightly/consumer.sh" "$@"
}

hostile_gopath="$tmp/hostile-gopath"
ambient_gobin="$tmp/ambient-gobin"
mkdir -p "$hostile_gopath/first/bin" "$hostile_gopath/second/bin" "$ambient_gobin" "$tmp/hostile-tools/bin"
cat > "$tmp/hostile-tools/brew" <<'BREW'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == --prefix && "${2:-}" == go@1.25 ]] || exit 2
printf '%s\n' "${MOCK_GO_PREFIX:?}"
BREW
cat > "$tmp/hostile-tools/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  env)
    [[ "${2:-}" == GOPATH ]] || exit 2
    printf '%s\n' "${MOCK_GOPATH:?}"
    ;;
  install)
    [[ "${GOBIN:-}" == "${MOCK_GOPATH%%:*}/bin" ]] || {
      echo "xcaddy was not installed in the first GOPATH entry: ${GOBIN:-<unset>}" >&2
      exit 1
    }
    mkdir -p "$GOBIN"
    : > "$GOBIN/xcaddy"
    ;;
  *) exit 2 ;;
esac
GO
chmod +x "$tmp/hostile-tools/brew" "$tmp/hostile-tools/bin/go"
hostile_result=$(
  PATH="$tmp/hostile-tools:$PATH" \
  GOBIN="$ambient_gobin" \
  GOPATH="$hostile_gopath/first:$hostile_gopath/second" \
  MOCK_GO_PREFIX="$tmp/hostile-tools" \
  MOCK_GOPATH="$hostile_gopath/first:$hostile_gopath/second" \
  bash -c '
    set -euo pipefail
    GO_BIN="$(brew --prefix go@1.25)/bin/go"
    XCADDY_BIN="$("$GO_BIN" env GOPATH | cut -d: -f1)/bin"
    GOBIN="$XCADDY_BIN" "$GO_BIN" install github.com/caddyserver/xcaddy/cmd/xcaddy@v0.4.6
    export PATH="$(brew --prefix go@1.25)/bin:$XCADDY_BIN:$PATH"
    printf "%s\n%s\n%s\n" "$GOBIN" "$PATH" "$(command -v xcaddy)"
  '
)
grep -Fq "$tmp/hostile-tools/bin:$hostile_gopath/first/bin:" <<<"$hostile_result"
grep -Fqx "$hostile_gopath/first/bin/xcaddy" <<<"$hostile_result"
[[ ! -e "$hostile_gopath/second/bin/xcaddy" && ! -e "$ambient_gobin/xcaddy" ]] || {
  echo 'hostile GOBIN/multi-GOPATH fixture wrote xcaddy to the wrong destination' >&2
  exit 1
}

first=$(make_mock_tools first)
run_consumer "$first" tap "$version" "$tag" "$sha256"
grep -Fxq 'brew update' "$first/calls"
grep -Fxq "brew install gordonbeeming/tap/shunt-nightly" "$first/calls"
grep -Fq 'curl --fail' "$first/calls"
grep -Fxq 'brew --prefix go@1.25' "$first/calls"
grep -Fxq 'go version' "$first/calls"
first_curl_line=$(grep -n -m1 '^curl ' "$first/calls" | cut -d: -f1)
first_update_line=$(grep -n -m1 -Fx 'brew update' "$first/calls" | cut -d: -f1)
first_install_line=$(grep -n -m1 -Fx 'brew install gordonbeeming/tap/shunt-nightly' "$first/calls" | cut -d: -f1)
[[ -n "$first_curl_line" && -n "$first_update_line" && -n "$first_install_line" && \
  "$first_curl_line" -lt "$first_update_line" && "$first_update_line" -lt "$first_install_line" ]] || {
  echo 'named consumer refresh and install did not follow the anonymous release probe' >&2
  exit 1
}

accepted_125=$(make_mock_tools accepted-125)
MOCK_GO_VERSION='go version go1.25.13 darwin/arm64' run_consumer "$accepted_125" candidate "$version" "$tag" "$sha256"

accepted_later=$(make_mock_tools accepted-later)
MOCK_GO_VERSION='go version go1.25.99 darwin/arm64' run_consumer "$accepted_later" candidate "$version" "$tag" "$sha256"
grep -Fxq 'brew uninstall --force shunt-nightly' "$accepted_later/calls"
[[ -z "$(cat "$accepted_later/version")" ]] || {
  echo 'later Homebrew Go acceptance fixture left a receipt behind' >&2
  exit 1
}

stale_tap=$(make_mock_tools stale-tap "$previous")
stale_log="$tmp/stale-go.log"
if MOCK_GO_VERSION='go version go1.25.12 darwin/arm64' run_consumer "$stale_tap" tap "$version" "$tag" "$sha256" "$previous" "$previous_tag" "$previous_sha256" 2>"$stale_log"; then
  echo 'stale Go named-tap fixture unexpectedly succeeded' >&2
  exit 1
fi
grep -Fq 'identity "go version go1.25.12 darwin/arm64"' "$stale_log"
grep -Fxq "brew upgrade gordonbeeming/tap/shunt-nightly" "$stale_tap/calls"
if grep -Eq '^shunt-nightly (version|skill) ' "$stale_tap/calls"; then
  echo 'stale Go named-tap fixture reached Shunt consumer checks' >&2
  exit 1
fi

unsupported_go=''
rejected_index=0
for unsupported_go in \
  'go version go1.25.12 darwin/arm64' \
  'go version go1.25.13rc1 darwin/arm64' \
  'go version go1.025.13 darwin/arm64' \
  'go version go1.25.013 darwin/arm64' \
  'go version go1.26.6 darwin/arm64' \
  'go version go1.27.0 darwin/arm64' \
  'go version go2.0.0 darwin/arm64' \
  'devel go1.27-deadbeef darwin/arm64' \
  'go version go1.25.13 linux/arm64'; do
  rejected_index=$((rejected_index + 1))
  rejected=$(make_mock_tools "rejected-$rejected_index")
  if MOCK_GO_VERSION="$unsupported_go" run_consumer "$rejected" candidate "$version" "$tag" "$sha256"; then
    echo "unsupported Homebrew Go fixture unexpectedly succeeded: $unsupported_go" >&2
    exit 1
  fi
  grep -Fxq 'brew install shunt/nightly-candidate/shunt-nightly' "$rejected/calls" || {
    echo "unsupported Homebrew Go did not install the candidate formula: $unsupported_go" >&2
    exit 1
  }
  if grep -Eq '^brew test |^shunt-nightly (version|skill) ' "$rejected/calls"; then
    echo "unsupported Homebrew Go reached candidate success checks: $unsupported_go" >&2
    exit 1
  fi
  [[ -z "$(cat "$rejected/version")" ]] || {
    echo "unsupported Homebrew Go cleanup left a receipt: $unsupported_go" >&2
    exit 1
  }
done

seeded=$(make_mock_tools seeded)
run_consumer "$seeded" tap "$version" "$tag" "$sha256" "$previous" "$previous_tag" "$previous_sha256"
grep -Fq 'brew install ' "$seeded/calls"
grep -Fxq 'brew untap --force gordonbeeming/tap' "$seeded/calls"
grep -Fxq 'brew update' "$seeded/calls"
grep -Fxq "brew upgrade gordonbeeming/tap/shunt-nightly" "$seeded/calls"

persistent=$(make_mock_tools persistent "$previous")
run_consumer "$persistent" tap "$version" "$tag" "$sha256" "$previous" "$previous_tag" "$previous_sha256"
if grep -Fq 'brew install ' "$persistent/calls"; then
  echo 'persistent prior consumer unexpectedly reinstalled the prior formula' >&2
  exit 1
fi
grep -Fxq 'brew update' "$persistent/calls"
grep -Fxq "brew upgrade gordonbeeming/tap/shunt-nightly" "$persistent/calls"

rerun=$(make_mock_tools rerun "$version")
run_consumer "$rerun" tap "$version" "$tag" "$sha256" "$version" "$tag" "$sha256"
grep -Fxq 'brew untap --force gordonbeeming/tap' "$rerun/calls"
grep -Fxq 'brew update' "$rerun/calls"
if grep -Eq '^brew (install|upgrade) ' "$rerun/calls"; then
  echo 'named rerun unexpectedly installed or upgraded the current nightly' >&2
  exit 1
fi

invalid_previous=$(make_mock_tools invalid-previous)
expect_failure run_consumer "$invalid_previous" tap "$version" "$tag" "$sha256" "$previous" "" ""
expect_failure run_consumer "$invalid_previous" tap "$version" "$tag" "$sha256" "$previous" nightly-99 "$previous_sha256"
expect_failure run_consumer "$invalid_previous" tap "$version" "$tag" "$sha256" "$previous" "$previous_tag" not-a-checksum

candidate=$(make_mock_tools candidate)
run_consumer "$candidate" candidate "$version" "$tag" "$sha256"
grep -Fq 'curl --fail' "$candidate/calls"
grep -Fq ' --output ' "$candidate/calls"
grep -Fxq 'brew update' "$candidate/calls"
grep -Fxq 'brew install shunt/nightly-candidate/shunt-nightly' "$candidate/calls"
grep -Fxq 'brew test shunt/nightly-candidate/shunt-nightly' "$candidate/calls"
grep -Fxq 'brew untap --force shunt/nightly-candidate' "$candidate/calls"
grep -Fxq 'brew --prefix' "$candidate/calls"
grep -Fxq 'brew --prefix shunt-nightly' "$candidate/calls"
grep -Fxq 'shunt-nightly version' "$candidate/calls"
grep -Fxq 'shunt-nightly skill install --all' "$candidate/calls"
grep -Fxq 'brew uninstall --force shunt-nightly' "$candidate/calls"
[[ -z "$(cat "$candidate/version")" ]] || {
  echo 'candidate validation left a Homebrew receipt behind' >&2
  exit 1
}
curl_line=$(grep -n -m1 '^curl .* --output ' "$candidate/calls" | cut -d: -f1)
update_line=$(grep -n -m1 -Fx 'brew update' "$candidate/calls" | cut -d: -f1)
install_line=$(grep -n -m1 -Fx 'brew install shunt/nightly-candidate/shunt-nightly' "$candidate/calls" | cut -d: -f1)
[[ -n "$curl_line" && -n "$update_line" && -n "$install_line" && "$curl_line" -lt "$update_line" && "$update_line" -lt "$install_line" ]] || {
  echo 'candidate Homebrew refresh and install did not follow the anonymous archive digest check' >&2
  exit 1
}

candidate_digest_failure=$(make_mock_tools candidate-digest-failure)
if PATH="$candidate_digest_failure/bin:$PATH" MOCK_BREW_STATE="$candidate_digest_failure" MOCK_TARGET_VERSION="$version" \
  MOCK_CURL_PAYLOAD=wrong-payload MOCK_GO_VERSION='go version go1.25.13 darwin/arm64' SHUNT_NIGHTLY_CURL=curl \
  "$root/packaging/nightly/consumer.sh" candidate "$version" "$tag" "$sha256"; then
  echo 'candidate archive digest mismatch fixture unexpectedly succeeded' >&2
  exit 1
fi
if grep -Eq '^brew (install|test) ' "$candidate_digest_failure/calls"; then
  echo 'candidate archive digest mismatch reached Homebrew installation' >&2
  exit 1
fi

candidate_rerun=$(make_mock_tools candidate-rerun "$version")
run_consumer "$candidate_rerun" candidate "$version" "$tag" "$sha256"
[[ "$(grep -Fc 'brew uninstall --force shunt-nightly' "$candidate_rerun/calls")" == 2 ]] || {
  echo 'candidate rerun did not clean both the stale and newly validated installs' >&2
  exit 1
}
[[ -z "$(cat "$candidate_rerun/version")" ]] || {
  echo 'candidate rerun left a Homebrew receipt behind' >&2
  exit 1
}

candidate_test_failure=$(make_mock_tools candidate-test-failure)
if PATH="$candidate_test_failure/bin:$PATH" MOCK_BREW_STATE="$candidate_test_failure" MOCK_TARGET_VERSION="$version" \
  MOCK_BREW_TEST_FAIL=true MOCK_GO_VERSION='go version go1.25.13 darwin/arm64' SHUNT_NIGHTLY_CURL=curl \
  "$root/packaging/nightly/consumer.sh" candidate "$version" "$tag" "$sha256"; then
  echo 'candidate Homebrew test failure fixture unexpectedly succeeded' >&2
  exit 1
fi
grep -Fxq 'brew uninstall --force shunt-nightly' "$candidate_test_failure/calls"
[[ -z "$(cat "$candidate_test_failure/version")" ]] || {
  echo 'failed candidate validation left a Homebrew receipt behind' >&2
  exit 1
}

probe_failure=$(make_mock_tools probe-failure)
if PATH="$probe_failure/bin:$PATH" MOCK_BREW_STATE="$probe_failure" MOCK_TARGET_VERSION="$version" MOCK_CURL_FAIL=true SHUNT_NIGHTLY_CURL=curl \
  "$root/packaging/nightly/consumer.sh" candidate "$version" "$tag" "$sha256"; then
  echo 'anonymous probe fixture unexpectedly succeeded' >&2
  exit 1
fi

echo 'nightly consumer fixtures passed'
