#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
goreleaser=${GORELEASER:-goreleaser}

[[ "$(uname -s)" == Darwin && "$(uname -m)" == arm64 ]] || {
  echo "official releases require Apple Silicon macOS with Rosetta" >&2
  exit 1
}
[[ -n "${CODESIGN_IDENTITY:-}" ]] || {
  echo "CODESIGN_IDENTITY is required; run this through mac-release codesign-run" >&2
  exit 1
}
command -v "$goreleaser" >/dev/null || {
  echo "missing GoReleaser: $goreleaser" >&2
  exit 1
}
command -v gh >/dev/null || {
  echo "missing GitHub CLI: gh" >&2
  exit 1
}

github_token=${GITHUB_TOKEN:-${GH_TOKEN:-}}
if [[ -z "$github_token" ]]; then
  github_token=$(gh auth token 2>/dev/null) || {
    echo "GitHub authentication is required to create the draft release" >&2
    exit 1
  }
fi
[[ -n "$github_token" ]] || {
  echo "GitHub authentication returned an empty token" >&2
  exit 1
}

head_commit=$(git -C "$root" rev-parse HEAD)
tag=$(git -C "$root" describe --tags --exact-match 2>/dev/null) || {
  echo "HEAD is not an exact release tag" >&2
  exit 1
}
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || {
  echo "invalid release tag: $tag" >&2
  exit 1
}
tag_commit=$(git -C "$root" rev-parse "refs/tags/$tag^{commit}")
[[ "$head_commit" == "$tag_commit" ]] || {
  echo "HEAD does not match release tag $tag" >&2
  exit 1
}
[[ -z "$(git -C "$root" status --porcelain --untracked-files=normal)" ]] || {
  echo "release checkout is not clean" >&2
  exit 1
}
git -C "$root" tag -v "$tag" >/dev/null 2>&1 || {
  echo "release tag is not signed by a trusted git signing key: $tag" >&2
  exit 1
}
local_tag_object=$(git -C "$root" rev-parse "refs/tags/$tag")
remote_tag_object=$(git -C "$root" ls-remote --exit-code origin "refs/tags/$tag" | awk 'NR == 1 { print $1 }') || {
  echo "release tag is not published on origin: $tag" >&2
  exit 1
}
[[ -n "$remote_tag_object" && "$local_tag_object" == "$remote_tag_object" ]] || {
  echo "origin tag object does not match the verified local tag: $tag" >&2
  exit 1
}
remote_tag_commit=$(git -C "$root" ls-remote --exit-code origin "refs/tags/$tag^{}" | awk 'NR == 1 { print $1 }') || {
  echo "origin tag is not an annotated tag: $tag" >&2
  exit 1
}
[[ -n "$remote_tag_commit" && "$tag_commit" == "$remote_tag_commit" ]] || {
  echo "origin tag commit does not match HEAD: $tag" >&2
  exit 1
}

cd "$root"
export GOWORK=off
export GITHUB_TOKEN=$github_token
export SLACRAWL_RELEASE_SIGNING_REQUIRED=1
exec "$goreleaser" release --clean
