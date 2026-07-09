#!/usr/bin/env bash
set -euo pipefail

binary=${1:-}
target=${2:-}
version=${3:-}
identifier=${SLACRAWL_CODESIGN_IDENTIFIER:-org.openclaw.slacrawl}
expected_team_id=${SLACRAWL_CODESIGN_TEAM_ID:-FWJYW4S8P8}
signing_required=${SLACRAWL_RELEASE_SIGNING_REQUIRED:-0}
requirement="identifier \"$identifier\" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"$expected_team_id\""

case "$signing_required" in
  0|1) ;;
  *)
    echo "SLACRAWL_RELEASE_SIGNING_REQUIRED must be 0 or 1" >&2
    exit 2
    ;;
esac

case "$target" in
  darwin_arm64*) expected_arch=arm64 ;;
  darwin_amd64*) expected_arch=x86_64 ;;
  *) exit 0 ;;
esac

[[ -f "$binary" ]] || {
  echo "GoReleaser binary does not exist: $binary" >&2
  exit 1
}

if [[ "$(uname -s)" != Darwin || -z "${CODESIGN_IDENTITY:-}" ]]; then
  if [[ "$signing_required" == 1 ]]; then
    echo "official macOS assets require a managed Developer ID identity on macOS" >&2
    exit 1
  fi
  echo "skipping Developer ID signing for local/cross-platform build: $target"
  exit 0
fi

for tool in codesign lipo; do
  command -v "$tool" >/dev/null || {
    echo "missing required tool: $tool" >&2
    exit 1
  }
done

codesign --force --options runtime --timestamp \
  --identifier "$identifier" --sign "$CODESIGN_IDENTITY" "$binary"
codesign --verify --strict -R="$requirement" --verbose=2 "$binary"

signature=$(codesign -dvvv "$binary" 2>&1)
grep -Fx "Identifier=$identifier" <<<"$signature" >/dev/null
grep -Fx "TeamIdentifier=$expected_team_id" <<<"$signature" >/dev/null
grep -F "Authority=Developer ID Application:" <<<"$signature" >/dev/null
lipo -archs "$binary" | tr ' ' '\n' | grep -Fx "$expected_arch" >/dev/null
[[ "$($binary --version)" == "$version" ]] || {
  echo "signed $target binary does not report release version $version" >&2
  exit 1
}
