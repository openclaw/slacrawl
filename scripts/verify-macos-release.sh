#!/usr/bin/env bash
set -euo pipefail

tag=${1:-}
checksum_file=${2:-}
shift 2 || true
identifier=${SLACRAWL_CODESIGN_IDENTIFIER:-org.openclaw.slacrawl.slacrawl}
expected_team_id=${SLACRAWL_CODESIGN_TEAM_ID:-FWJYW4S8P8}
requirement="identifier \"$identifier\" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"$expected_team_id\""

if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ || "$#" -eq 0 ]]; then
  echo "usage: $0 vX.Y.Z SHA256SUMS slacrawl_X.Y.Z_darwin_ARCH.tar.gz [...]" >&2
  exit 2
fi
[[ "$(uname -s)" == Darwin ]] || {
  echo "macOS signature verification must run on macOS" >&2
  exit 1
}
[[ -f "$checksum_file" ]] || {
  echo "missing checksum manifest: $checksum_file" >&2
  exit 1
}

version=${tag#v}
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/slacrawl-verify.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT

for archive in "$@"; do
  archive=$(cd "$(dirname "$archive")" && pwd)/$(basename "$archive")
  name=$(basename "$archive")
  case "$name" in
    "slacrawl_${version}_darwin_arm64.tar.gz") expected_arch=arm64 ;;
    "slacrawl_${version}_darwin_amd64.tar.gz") expected_arch=x86_64 ;;
    *)
      echo "unexpected macOS artifact name: $name" >&2
      exit 1
      ;;
  esac

  expected_checksum=$(awk -v name="$name" '$2 == name { print $1 }' "$checksum_file")
  [[ "$expected_checksum" =~ ^[0-9a-fA-F]{64}$ ]] || {
    echo "missing or invalid checksum for $name" >&2
    exit 1
  }
  actual_checksum=$(shasum -a 256 "$archive" | awk '{ print $1 }')
  [[ "$actual_checksum" == "$expected_checksum" ]] || {
    echo "checksum mismatch for $name" >&2
    exit 1
  }

  binary="$work_dir/slacrawl-$expected_arch"
  tar -xOzf "$archive" slacrawl > "$binary"
  chmod 0755 "$binary"

  codesign --verify --strict --check-notarization -R=notarized --verbose=2 "$binary"
  codesign --verify --strict -R="$requirement" --verbose=2 "$binary"
  signature=$(codesign -dvvv "$binary" 2>&1)
  grep -Fx "Identifier=$identifier" <<<"$signature" >/dev/null
  grep -Fx "TeamIdentifier=$expected_team_id" <<<"$signature" >/dev/null
  grep -F "Authority=Developer ID Application:" <<<"$signature" >/dev/null
  lipo -archs "$binary" | tr ' ' '\n' | grep -Fx "$expected_arch" >/dev/null
  [[ "$($binary --version)" == "$version" ]] || {
    echo "$name does not report release version $version" >&2
    exit 1
  }
done
