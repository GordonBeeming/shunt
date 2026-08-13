#!/usr/bin/env bash
# Validate the nightly distribution from both sides of the tap publication gate.
set -euo pipefail

tap=gordonbeeming/tap
formula_name=shunt-nightly
asset=shunt-nightly_darwin_arm64.tar.gz

usage() {
  cat >&2 <<'USAGE'
usage:
  consumer.sh candidate VERSION TAG SHA256 [--fixture]
  consumer.sh tap VERSION TAG SHA256 [PREVIOUS_VERSION PREVIOUS_TAG PREVIOUS_SHA256] [--fixture]

candidate validates the rendered candidate formula before tap publication.
tap renders and installs the exact prior public formula on a clean runner,
freshly retaps the named formula, then validates an upgrade to the candidate.

The legacy `consumer.sh VERSION TAG SHA256 [--fixture]` form means candidate.
USAGE
  exit 2
}

mode=candidate
if [[ $# -gt 0 && ("$1" == candidate || "$1" == tap) ]]; then
  mode=$1
  shift
fi

[[ $# -ge 3 ]] || usage
version=$1
tag=$2
sha256=$3
shift 3
previous_version=""
previous_tag=""
previous_sha256=""
fixture=false
case "$mode" in
  candidate)
    if [[ $# -eq 1 && "$1" == --fixture ]]; then
      fixture=true
    elif [[ $# -ne 0 ]]; then
      usage
    fi
    ;;
  tap)
    if [[ $# -eq 1 && "$1" == --fixture ]]; then
      fixture=true
    elif [[ $# -eq 3 || $# -eq 4 ]]; then
      previous_version=$1
      previous_tag=$2
      previous_sha256=$3
      if [[ $# -eq 4 && "$4" == --fixture ]]; then
        fixture=true
      elif [[ $# -eq 4 ]]; then
        usage
      fi
    elif [[ $# -ne 0 ]]; then
      usage
    fi
    ;;
esac

[[ "$version" =~ ^0\.0\.0-nightly\.[0-9]+$ ]] || { echo "invalid nightly version: $version" >&2; exit 2; }
[[ "$tag" =~ ^nightly-[0-9]+$ ]] || { echo "invalid nightly tag: $tag" >&2; exit 2; }
[[ "$sha256" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid SHA-256: $sha256" >&2; exit 2; }
previous_metadata_present=false
if [[ -n "$previous_version$previous_tag$previous_sha256" ]]; then
  [[ -n "$previous_version" && -n "$previous_tag" && -n "$previous_sha256" ]] || {
    echo "previous nightly metadata must include version, tag, and SHA-256 together" >&2
    exit 2
  }
  [[ "$previous_version" =~ ^0\.0\.0-nightly\.[0-9]+$ ]] || { echo "invalid previous nightly version: $previous_version" >&2; exit 2; }
  [[ "$previous_tag" =~ ^nightly-[0-9]+$ ]] || { echo "invalid previous nightly tag: $previous_tag" >&2; exit 2; }
  [[ "$previous_sha256" =~ ^[0-9a-f]{64}$ ]] || { echo "invalid previous SHA-256: $previous_sha256" >&2; exit 2; }
  [[ "$previous_version" == "0.0.0-nightly.${previous_tag#nightly-}" ]] || {
    echo "previous nightly version and tag disagree" >&2
    exit 2
  }
  previous_metadata_present=true
fi

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
candidate_formula="$tmp/shunt-nightly.rb"
"$root/packaging/homebrew/render.sh" "$version" "$tag" "$sha256" "$candidate_formula"
previous_formula="$tmp/previous-shunt-nightly.rb"

assert_candidate_formula() {
  grep -Fqx "  version \"$version\"" "$candidate_formula"
  grep -Fqx "  sha256 \"$sha256\"" "$candidate_formula"
  grep -Fqx "  url \"https://github.com/GordonBeeming/shunt/releases/download/$tag/$asset\"" "$candidate_formula"
}

anonymous_release_probe() {
  local url="https://github.com/GordonBeeming/shunt/releases/download/$tag/$asset"
  local curl_bin=${SHUNT_NIGHTLY_CURL:-curl}
  echo "probing anonymous release asset: $url"
  # Deliberately clear CI credentials: a formula must work for an unauthenticated
  # Homebrew consumer, not merely for the release workflow's token.
  env -u GH_TOKEN -u GITHUB_TOKEN "$curl_bin" --fail --silent --show-error --location --range 0-0 --max-time 30 "$url" >/dev/null
}

download_anonymous_release() {
  local url="https://github.com/GordonBeeming/shunt/releases/download/$tag/$asset"
  local archive="$tmp/$asset"
  local actual curl_bin=${SHUNT_NIGHTLY_CURL:-curl}
  echo "downloading anonymous release asset: $url"
  # Candidate validation runs on hosted macOS 15. It verifies the public bytes
  # without trying to install or execute an arm64/macOS-26+-only formula there.
  env -u GH_TOKEN -u GITHUB_TOKEN "$curl_bin" --fail --silent --show-error --location --max-time 180 --output "$archive" "$url"
  actual=$(shasum -a 256 "$archive" | awk '{print $1}')
  [[ "$actual" == "$sha256" ]] || { printf 'anonymous archive digest mismatch: expected=%s actual=%s\n' "$sha256" "$actual" >&2; exit 1; }
}

installed_receipt() {
  brew list --versions "$formula_name" 2>/dev/null || true
}

assert_absent() {
  local receipt
  receipt=$(installed_receipt)
  [[ -z "$receipt" ]] || { echo "expected no $formula_name install, found: $receipt" >&2; exit 1; }
}

uninstall_existing() {
  if [[ -n "$(installed_receipt)" ]]; then
    brew uninstall --force "$formula_name"
  fi
  assert_absent
}

assert_installed_consumer() {
  local receipt stable installed_binary version_output skill_home skill_root
  stable="$(brew --prefix)/bin/$formula_name"
  receipt=$(installed_receipt)
  [[ "$receipt" == "$formula_name $version" ]] || { printf 'unexpected Homebrew receipt: expected=%s actual=%s\n' "$formula_name $version" "$receipt" >&2; exit 1; }
  installed_binary="$(brew --prefix "$formula_name")/bin/$formula_name"
  [[ -L "$stable" && -x "$stable" && -x "$installed_binary" && "$stable" -ef "$installed_binary" ]] || {
    echo "stable Homebrew binary is not the exact installed path: $stable -> $installed_binary" >&2
    exit 1
  }
  version_output=$("$stable" version)
  grep -Fq 'channel=nightly' <<<"$version_output"
  grep -Fq 'binary=shunt-nightly' <<<"$version_output"
  grep -Fq "version=$version" <<<"$version_output"

  # Exercise the released embedded renderer in a disposable home, never an
  # agent's real skill directory.
  skill_home="$tmp/home"
  mkdir -p "$skill_home/.claude" "$skill_home/.codex" "$skill_home/.config/opencode"
  HOME="$skill_home" "$stable" skill install --all
  skill_root="$skill_home/.codex/skills/shunt"
  [[ -d "$skill_root" ]] || { echo "nightly skill was not installed" >&2; exit 1; }
  if grep -R -nF '{{shunt-command}}' "$skill_root"; then
    echo "skill retained an unresolved command placeholder" >&2
    exit 1
  fi
  grep -R -nF 'shunt-nightly ' "$skill_root" >/dev/null
  grep -R -nF '.shunt-dev' "$skill_root" >/dev/null
}

refresh_named_tap() {
  if brew tap | grep -Fxq "$tap"; then
    brew untap --force "$tap"
  fi
  brew tap "$tap"
}

has_distinct_prior_release() {
  [[ "$previous_metadata_present" == true && \
    ( "$previous_version" != "$version" || "$previous_tag" != "$tag" || "$previous_sha256" != "$sha256" ) ]]
}

seed_prior_formula_if_clean() {
  local receipt
  if ! has_distinct_prior_release; then
    return 0
  fi
  "$root/packaging/homebrew/render.sh" "$previous_version" "$previous_tag" "$previous_sha256" "$previous_formula"
  receipt=$(installed_receipt)
  if [[ -z "$receipt" ]]; then
    echo "installing exact prior public nightly before named-tap upgrade"
    brew install "$previous_formula"
    brew test "$previous_formula"
    receipt=$(installed_receipt)
    [[ "$receipt" == "$formula_name $previous_version" ]] || {
      printf 'prior formula install produced unexpected receipt: expected=%s actual=%s\n' "$formula_name $previous_version" "$receipt" >&2
      exit 1
    }
  elif [[ "$receipt" != "$formula_name $previous_version" && "$receipt" != "$formula_name $version" ]]; then
    printf 'expected a clean consumer or known nightly receipt, found: %s\n' "$receipt" >&2
    exit 1
  fi
}

assert_candidate_formula
if [[ "$fixture" == true ]]; then
  echo "nightly consumer fixture passed: mode=$mode formula=$version tag=$tag sha256=$sha256"
  exit 0
fi

case "$mode" in
  candidate)
    download_anonymous_release
    ;;
  tap)
    command -v brew >/dev/null || { echo "brew is required on the macOS consumer runner" >&2; exit 1; }
    anonymous_release_probe
    seed_prior_formula_if_clean
    refresh_named_tap
    receipt=$(installed_receipt)
    if [[ -z "$receipt" ]]; then
      brew install "$tap/$formula_name"
      brew test "$tap/$formula_name"
    elif has_distinct_prior_release && [[ "$receipt" == "$formula_name $previous_version" ]]; then
      brew update
      brew upgrade "$tap/$formula_name"
    elif [[ "$receipt" == "$formula_name $version" ]]; then
      echo "named consumer found the exact current receipt; re-verifying it"
    else
      printf 'unexpected Homebrew receipt after fresh retap: %s\n' "$receipt" >&2
      exit 1
    fi
    ;;
esac

if [[ "$mode" == tap ]]; then
  assert_installed_consumer
fi
echo "nightly consumer validation passed: mode=$mode version=$version"
