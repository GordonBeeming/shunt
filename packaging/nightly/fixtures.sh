#!/usr/bin/env bash
# Hermetic state-machine fixtures for nightly release recovery.
set -euo pipefail

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
commit=0123456789abcdef0123456789abcdef01234567
tag=nightly-9
version=0.0.0-nightly.9
asset=shunt-nightly_darwin_arm64.tar.gz
checksum="$asset.sha256"

expect_failure() {
  if "$@"; then
    echo "fixture unexpectedly succeeded: $*" >&2
    exit 1
  fi
}

make_payload() {
  local directory=$1 body=${2:-payload}
  mkdir -p "$directory"
  printf '%s' "$body" > "$directory/$asset"
  local digest
  digest=$(shasum -a 256 "$directory/$asset" | awk '{print $1}')
  printf '%s  %s\n' "$digest" "$asset" > "$directory/$checksum"
}

make_mock_gh() {
  local directory=$1
  mkdir -p "$directory/bin" "$directory/remote-assets"
  cat > "$directory/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
state=${MOCK_RELEASE_STATE:?}
assets=${MOCK_RELEASE_ASSETS:?}
calls=${MOCK_RELEASE_CALLS:?}
mode=${MOCK_RELEASE_MODE:-none}
tag=${MOCK_RELEASE_TAG:?}
commit=${MOCK_RELEASE_COMMIT:?}
record() { printf '%s\n' "$*" >> "$calls"; }
response() {
  local code=$1
  printf 'HTTP/2 %s\n\n' "$code"
  [[ "$code" == 200 ]] && cat "$state"
}
release_exists() { [[ -s "$state" ]]; }
asset_names() { jq -r '.assets[]?.name' "$state"; }
add_asset() {
  local name=$1
  jq --arg name "$name" '.assets += [{name:$name}]' "$state" > "$state.tmp"
  mv "$state.tmp" "$state"
}
case "${1:-}" in
  api)
    shift
    if [[ "${1:-}" == -i ]]; then
      shift
      record "api-read $1"
      if release_exists; then response 200; exit 0; fi
      response 404
      exit 1
    fi
    if [[ "${1:-}" == --paginate ]]; then
      record 'api-list'
      printf '[[%s]]\n' "$(release_exists && cat "$state" || printf '')"
      exit 0
    fi
    echo "unexpected gh api invocation: $*" >&2
    exit 2
    ;;
  release)
    shift
    case "${1:-}" in
      create)
        record 'release create'
        cat > "$state" <<JSON
{"tag_name":"$tag","target_commitish":"$commit","prerelease":true,"draft":true,"immutable":false,"assets":[]}
JSON
        [[ "$mode" != create-after-success ]] || exit 1
        ;;
      upload)
        shift
        release_tag=$1
        upload_file=$2
        [[ "$release_tag" == "$tag" ]] || exit 2
        name=$(basename -- "$upload_file")
        record "release upload $name"
        cp "$upload_file" "$assets/$name"
        add_asset "$name"
        [[ "$mode" != upload-after-success ]] || exit 1
        ;;
      download)
        shift
        release_tag=$1
        shift
        [[ "$release_tag" == "$tag" ]] || exit 2
        destination=''
        patterns=()
        while [[ $# -gt 0 ]]; do
          case "$1" in
            --dir) destination=$2; shift 2 ;;
            --pattern) patterns+=("$2"); shift 2 ;;
            --clobber) shift ;;
            *) echo "unexpected gh release download option: $1" >&2; exit 2 ;;
          esac
        done
        record "release download ${patterns[*]}"
        mkdir -p "$destination"
        for pattern in "${patterns[@]}"; do cp "$assets/$pattern" "$destination/$pattern"; done
        ;;
      edit)
        shift
        release_tag=$1
        shift
        [[ "$release_tag" == "$tag" && "${1:-}" == --draft=false ]] || exit 2
        record 'release publish'
        if [[ "$mode" == publish-mutable* ]]; then
          jq '.draft = false | .immutable = false' "$state" > "$state.tmp"
        else
          jq '.draft = false | .immutable = true' "$state" > "$state.tmp"
        fi
        mv "$state.tmp" "$state"
        if [[ "$mode" == publish-swaps-archive ]]; then
          printf 'replaced-at-immutable-lock' > "$assets/shunt-nightly_darwin_arm64.tar.gz"
        fi
        [[ "$mode" != publish-after-success ]] || exit 1
        ;;
      delete)
        shift
        release_tag=$1
        [[ "$release_tag" == "$tag" ]] || exit 2
        record 'release delete'
        : > "$state"
        [[ "$mode" != delete-after-success && "$mode" != publish-mutable-delete-after-success ]] || exit 1
        ;;
      *) echo "unexpected gh release invocation: $*" >&2; exit 2 ;;
    esac
    ;;
  *) echo "unexpected gh invocation: $*" >&2; exit 2 ;;
esac
MOCK
  chmod +x "$directory/bin/gh"
}

run_publisher() {
  local directory=$1 mode=$2
  PATH="$directory/bin:$PATH" \
    MOCK_RELEASE_STATE="$directory/state.json" \
    MOCK_RELEASE_ASSETS="$directory/remote-assets" \
    MOCK_RELEASE_CALLS="$directory/calls" \
    MOCK_RELEASE_MODE="$mode" \
    MOCK_RELEASE_TAG="$tag" \
    MOCK_RELEASE_COMMIT="$commit" \
    GITHUB_REPOSITORY=GordonBeeming/shunt \
    "$root/packaging/nightly/publish-release.sh" "$tag" "$version" "$commit" "$directory/payload"
}

prepare_case() {
  local name=$1 body=${2:-payload}
  local case_dir="$tmp/$name"
  mkdir -p "$case_dir"
  : > "$case_dir/calls"
  make_payload "$case_dir/payload" "$body"
  make_mock_gh "$case_dir"
  printf '%s' "$case_dir"
}

write_release() {
  local directory=$1 draft=$2 immutable=$3 target=${4:-$commit}
  shift 4 || true
  local asset_json='[]'
  if (($# > 0)); then
    asset_json=$(printf '%s\n' "$@" | jq -R . | jq -s .)
  fi
  jq -n --arg tag "$tag" --arg commit "$target" --argjson draft "$draft" --argjson immutable "$immutable" --argjson assets "$asset_json" \
    '{tag_name:$tag,target_commitish:$commit,prerelease:true,draft:$draft,immutable:$immutable,assets:[$assets[] | {name:.}]}' > "$directory/state.json"
}

# A new release takes the draft-first path and finishes only after the immutable
# response contains both candidate bytes.
clean=$(prepare_case clean)
run_publisher "$clean" none
jq -e '.draft == false and .immutable == true and ([.assets[].name] | sort == ["shunt-nightly_darwin_arm64.tar.gz","shunt-nightly_darwin_arm64.tar.gz.sha256"])' "$clean/state.json" >/dev/null
if rg -q 'immutable-releases|immutable-preflight' "$clean/calls"; then
  echo 'publisher attempted the admin-only immutable release settings preflight' >&2
  exit 1
fi

# Each mutation can fail after GitHub accepted it. The next read determines
# whether the exact remote operation already happened.
for ambiguous in create-after-success upload-after-success publish-after-success; do
  case_dir=$(prepare_case "$ambiguous")
  run_publisher "$case_dir" "$ambiguous"
  jq -e '.draft == false and .immutable == true' "$case_dir/state.json" >/dev/null
done

# The draft bytes can match before publication and still be replaced at the
# immutable lock boundary. The reconciler must re-download the locked asset,
# detect the replacement, and stop without another release mutation.
locked_replacement=$(prepare_case locked-replacement)
expect_failure run_publisher "$locked_replacement" publish-swaps-archive
pre_publish_calls=$(sed '/^release publish$/,$d' "$locked_replacement/calls")
grep -Fq "release download $asset $checksum" <<<"$pre_publish_calls" || {
  echo 'locked replacement did not authenticate the matching draft bytes' >&2
  exit 1
}
grep -Fxq 'release publish' "$locked_replacement/calls"
published_count=$(grep -Fxc 'release publish' "$locked_replacement/calls")
[[ "$published_count" == 1 ]] || {
  echo "locked replacement made $published_count publish requests" >&2
  exit 1
}

# A mutable post-publish response must delete only the authenticated candidate
# and fail, which prevents the workflow from passing it to the tap update.
mutable=$(prepare_case mutable)
expect_failure run_publisher "$mutable" publish-mutable
[[ ! -s "$mutable/state.json" ]] || { echo 'mutable published release was not deleted' >&2; exit 1; }
grep -Fxq 'release publish' "$mutable/calls"
grep -Fxq 'release delete' "$mutable/calls"
if rg -q 'immutable-releases|immutable-preflight' "$mutable/calls"; then
  echo 'mutable release recovery attempted the admin-only settings preflight' >&2
  exit 1
fi

# Deletion may have succeeded before its transport response failed. A 404 on
# the helper's retry read is a reconciled cleanup, but publication still fails.
mutable_deleted=$(prepare_case mutable-delete-after-success)
expect_failure run_publisher "$mutable_deleted" publish-mutable-delete-after-success
[[ ! -s "$mutable_deleted/state.json" ]] || { echo 'retry-safe mutable cleanup left a release state' >&2; exit 1; }
deleted_count=$(grep -Fxc 'release delete' "$mutable_deleted/calls")
[[ "$deleted_count" == 1 ]] || { echo "retry-safe mutable cleanup made $deleted_count delete requests" >&2; exit 1; }

# A mutable published candidate can survive an earlier delete response failure.
# Resume must reconcile that state too, including the already-deleted 404 case.
preexisting_mutable=$(prepare_case preexisting-mutable)
write_release "$preexisting_mutable" false false "$commit" "$asset" "$checksum"
cp "$preexisting_mutable/payload/$asset" "$preexisting_mutable/remote-assets/$asset"
cp "$preexisting_mutable/payload/$checksum" "$preexisting_mutable/remote-assets/$checksum"
expect_failure run_publisher "$preexisting_mutable" delete-after-success
[[ ! -s "$preexisting_mutable/state.json" ]] || { echo 'pre-existing mutable release was not deleted' >&2; exit 1; }
preexisting_deleted_count=$(grep -Fxc 'release delete' "$preexisting_mutable/calls")
[[ "$preexisting_deleted_count" == 1 ]] || { echo "pre-existing mutable cleanup made $preexisting_deleted_count delete requests" >&2; exit 1; }

# A matching partial draft is repairable. An existing mismatched byte is a hard
# fence and must not issue an upload or publish request.
partial=$(prepare_case partial)
write_release "$partial" true false "$commit" "$asset"
cp "$partial/payload/$asset" "$partial/remote-assets/$asset"
run_publisher "$partial" none
jq -e '.draft == false and .immutable == true' "$partial/state.json" >/dev/null

mismatch=$(prepare_case mismatch)
write_release "$mismatch" true false "$commit" "$asset"
printf tampered > "$mismatch/remote-assets/$asset"
expect_failure run_publisher "$mismatch" none
if rg -q 'release (upload|publish|create)' "$mismatch/calls"; then
  echo 'mismatched draft permitted a release mutation' >&2
  exit 1
fi

checksum_partial=$(prepare_case checksum-partial)
write_release "$checksum_partial" true false "$commit" "$checksum"
cp "$checksum_partial/payload/$checksum" "$checksum_partial/remote-assets/$checksum"
run_publisher "$checksum_partial" none
jq -e '.draft == false and .immutable == true' "$checksum_partial/state.json" >/dev/null

# A draft may be repaired only when every existing asset belongs to the exact
# archive/checksum set. An unexpected file must block before an upload or publish.
extra_draft=$(prepare_case extra-draft)
write_release "$extra_draft" true false "$commit" "$asset" unexpected.txt
cp "$extra_draft/payload/$asset" "$extra_draft/remote-assets/$asset"
expect_failure run_publisher "$extra_draft" none
if rg -q 'release (upload|publish|create)' "$extra_draft/calls"; then
  echo 'draft with an extra asset permitted a release mutation' >&2
  exit 1
fi

# An immutable release is resumable only when every invariant already matches.
immutable=$(prepare_case immutable)
write_release "$immutable" false true "$commit" "$asset" "$checksum"
cp "$immutable/payload/$asset" "$immutable/remote-assets/$asset"
cp "$immutable/payload/$checksum" "$immutable/remote-assets/$checksum"
run_publisher "$immutable" none
if rg -q 'release (create|upload|publish)' "$immutable/calls"; then
  echo 'matching immutable release was mutated' >&2
  exit 1
fi

immutable_extra=$(prepare_case immutable-extra)
write_release "$immutable_extra" false true "$commit" "$asset" "$checksum" unexpected.txt
cp "$immutable_extra/payload/$asset" "$immutable_extra/remote-assets/$asset"
cp "$immutable_extra/payload/$checksum" "$immutable_extra/remote-assets/$checksum"
expect_failure run_publisher "$immutable_extra" none
if rg -q 'release (create|upload|publish)' "$immutable_extra/calls"; then
  echo 'published immutable release with an extra asset was mutated' >&2
  exit 1
fi

invariant=$(prepare_case invariant)
write_release "$invariant" false true deadbeefdeadbeefdeadbeefdeadbeefdeadbeef "$asset" "$checksum"
cp "$invariant/payload/$asset" "$invariant/remote-assets/$asset"
cp "$invariant/payload/$checksum" "$invariant/remote-assets/$checksum"
expect_failure run_publisher "$invariant" none
if rg -q 'release (create|upload|publish)' "$invariant/calls"; then
  echo 'metadata mismatch permitted a release mutation' >&2
  exit 1
fi

wrong_tag=$(prepare_case wrong-tag)
write_release "$wrong_tag" true false "$commit"
jq '.tag_name = "nightly-10"' "$wrong_tag/state.json" > "$wrong_tag/state.tmp"
mv "$wrong_tag/state.tmp" "$wrong_tag/state.json"
expect_failure run_publisher "$wrong_tag" none
if rg -q 'release (create|upload|publish)' "$wrong_tag/calls"; then
  echo 'tag mismatch permitted a release mutation' >&2
  exit 1
fi

# Retention sees every page and leaves exactly the current release plus 29 newest.
python3 - "$tmp/releases.json" <<'PY'
import json, sys
releases = [{"tag_name": f"nightly-{n}", "prerelease": True, "created_at": f"2026-01-{(n % 28) + 1:02d}T00:00:00Z"} for n in range(1, 131)]
json.dump(releases, open(sys.argv[1], "w"))
PY
deleted=$("$root/packaging/nightly/prune-releases.sh" nightly-130 "$tmp/releases.json" | wc -l | tr -d ' ')
[[ "$deleted" == 100 ]] || { echo "retention selected $deleted releases, expected 100" >&2; exit 1; }
if "$root/packaging/nightly/prune-releases.sh" nightly-130 "$tmp/releases.json" | grep -Fxq nightly-130; then
  echo 'retention selected the current release' >&2
  exit 1
fi

# Delete treats a remote-success transport failure as complete when the follow-up
# read returns 404.
deleted_case=$(prepare_case deleted)
write_release "$deleted_case" false true "$commit" "$asset" "$checksum"
PATH="$deleted_case/bin:$PATH" MOCK_RELEASE_STATE="$deleted_case/state.json" MOCK_RELEASE_ASSETS="$deleted_case/remote-assets" MOCK_RELEASE_CALLS="$deleted_case/calls" MOCK_RELEASE_MODE=delete-after-success MOCK_RELEASE_TAG="$tag" MOCK_RELEASE_COMMIT="$commit" GITHUB_REPOSITORY=GordonBeeming/shunt \
  "$root/packaging/nightly/delete-release.sh" "$tag"
[[ ! -s "$deleted_case/state.json" ]] || { echo 'already-complete deletion left a release state' >&2; exit 1; }

# Selection carries the published formula metadata for the later consumer
# upgrade check, while a first nightly has no prior release to upgrade.
printf '[{"tag_name":"nightly-7","target_commitish":"%s","prerelease":true}]\n' "$commit" > "$tmp/selection.json"
first_selection=$(GITHUB_SHA=$commit "$root/packaging/nightly/select-release.sh" false 12 "$tmp/selection.json")
grep -Fxq 'version=0.0.0-nightly.7' <<<"$first_selection"
for empty_previous in previous_version= previous_tag= previous_sha256=; do
  grep -Fxq "$empty_previous" <<<"$first_selection" || {
    echo "first nightly selection omitted $empty_previous" >&2
    exit 1
  }
done
if grep -q '^tap_version=' <<<"$first_selection"; then
  echo 'selection still emits tap_version' >&2
  exit 1
fi

# macOS ships BSD sort, which has no GNU `-V` option. Select the greatest
# numeric nightly suffix so selection remains portable across local and hosted runs.
printf '[{"tag_name":"nightly-9","target_commitish":"%s","prerelease":true},{"tag_name":"nightly-10","target_commitish":"%s","prerelease":true}]\n' "$commit" "$commit" > "$tmp/selection.json"
numeric_selection=$(GITHUB_SHA=$commit "$root/packaging/nightly/select-release.sh" false 12 "$tmp/selection.json")
grep -Fxq 'tag=nightly-10' <<<"$numeric_selection"

formula="$tmp/Formula/shunt-nightly.rb"
mkdir -p "$(dirname -- "$formula")"
previous_sha256=$(printf 'a%.0s' {1..64})
cat > "$formula" <<FORMULA
class ShuntNightly < Formula
  version "0.0.0-nightly.6"
  url "https://github.com/GordonBeeming/shunt/releases/download/nightly-6/shunt-nightly_darwin_arm64.tar.gz"
  sha256 "$previous_sha256"
end
FORMULA
prior_selection=$(GITHUB_SHA=$commit TAP_FORMULA="$formula" "$root/packaging/nightly/select-release.sh" false 12 "$tmp/selection.json")
grep -Fxq 'previous_version=0.0.0-nightly.6' <<<"$prior_selection"
grep -Fxq 'previous_tag=nightly-6' <<<"$prior_selection"
grep -Fxq "previous_sha256=$previous_sha256" <<<"$prior_selection"

# A 404 is a valid lookup result. All other failed reads receive bounded retries.
not_found=$(prepare_case not-found)
PATH="$not_found/bin:$PATH" MOCK_RELEASE_STATE="$not_found/state.json" MOCK_RELEASE_ASSETS="$not_found/remote-assets" MOCK_RELEASE_CALLS="$not_found/calls" MOCK_RELEASE_TAG="$tag" MOCK_RELEASE_COMMIT="$commit" \
  "$root/packaging/nightly/read-release.sh" "$tmp/not-found.response" "repos/GordonBeeming/shunt/releases/tags/$tag"
grep -Fqx 'HTTP/2 404' "$tmp/not-found.response"
expect_failure "$root/packaging/nightly/retry.sh" 1 false

release_list=$(prepare_case release-list)
write_release "$release_list" true false "$commit"
PATH="$release_list/bin:$PATH" MOCK_RELEASE_STATE="$release_list/state.json" MOCK_RELEASE_ASSETS="$release_list/remote-assets" MOCK_RELEASE_CALLS="$release_list/calls" MOCK_RELEASE_TAG="$tag" MOCK_RELEASE_COMMIT="$commit" GITHUB_REPOSITORY=GordonBeeming/shunt \
  "$root/packaging/nightly/read-releases.sh" "$tmp/all-releases.json"
jq -e 'length == 1 and .[0].tag_name == "nightly-9"' "$tmp/all-releases.json" >/dev/null

# The workflow delegates release mutation to the state reconciler, and the
# nightly fixture runs the distinct Homebrew consumer fixture in the same gate.
# shellcheck disable=SC2016 # GitHub runner variables are workflow literals.
grep -Fq 'packaging/nightly/publish-release.sh "$TAG" "$VERSION" "$GITHUB_SHA" "$RUNNER_TEMP/release-payload"' "$root/.github/workflows/nightly.yml"
if rg -q 'gh release (create|upload|edit)' "$root/.github/workflows/nightly.yml"; then
  echo 'workflow still contains inline release mutation' >&2
  exit 1
fi
"$root/packaging/homebrew/consumer-fixtures.sh"

echo 'nightly release fixtures passed'
