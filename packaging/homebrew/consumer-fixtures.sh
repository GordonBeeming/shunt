#!/usr/bin/env bash
# Hermetic coverage for consumer first-install, upgrade, and anonymous probes.
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

expect_failure() {
  if "$@"; then
    echo "fixture unexpectedly succeeded: $*" >&2
    exit 1
  fi
}

make_mock_tools() {
  local name=$1 installed=${2:-}
  local state="$tmp/$name"
  mkdir -p "$state/bin" "$state/cellar/bin"
  printf '%s' "$installed" > "$state/version"
  : > "$state/calls"
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
    if [[ $# -eq 1 ]]; then echo "$state"; else echo "$state/cellar"; fi
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
    [[ "${2:-}" == --force && "${3:-}" == gordonbeeming/tap ]]
    ;;
  tap)
    if [[ $# -eq 1 ]]; then printf '%s\n' gordonbeeming/tap; fi
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
  PATH="$state/bin:$PATH" MOCK_BREW_STATE="$state" MOCK_TARGET_VERSION="$version" SHUNT_NIGHTLY_CURL=curl \
    "$root/packaging/nightly/consumer.sh" "$@"
}

first=$(make_mock_tools first)
run_consumer "$first" tap "$version" "$tag" "$sha256"
grep -Fxq "brew install gordonbeeming/tap/shunt-nightly" "$first/calls"
grep -Fq 'curl --fail' "$first/calls"

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
if grep -Eq '^brew (install|upgrade|update)' "$rerun/calls"; then
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
grep -Eq '^brew install .*/shunt-nightly\.rb$' "$candidate/calls"
grep -Eq '^brew test .*/shunt-nightly\.rb$' "$candidate/calls"
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
install_line=$(grep -n -m1 -E '^brew install .*/shunt-nightly\.rb$' "$candidate/calls" | cut -d: -f1)
[[ -n "$curl_line" && -n "$install_line" && "$curl_line" -lt "$install_line" ]] || {
  echo 'candidate formula was installed before the anonymous archive digest check' >&2
  exit 1
}

candidate_digest_failure=$(make_mock_tools candidate-digest-failure)
if PATH="$candidate_digest_failure/bin:$PATH" MOCK_BREW_STATE="$candidate_digest_failure" MOCK_TARGET_VERSION="$version" \
  MOCK_CURL_PAYLOAD=wrong-payload SHUNT_NIGHTLY_CURL=curl \
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
  MOCK_BREW_TEST_FAIL=true SHUNT_NIGHTLY_CURL=curl \
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
