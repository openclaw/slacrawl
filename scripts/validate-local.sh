#!/usr/bin/env bash

set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

cd "$root_dir"

if [[ -z "${SLACK_BOT_TOKEN:-}" ]]; then
  echo "SLACK_BOT_TOKEN is required" >&2
  exit 1
fi

workspace_id="${SLACRAWL_WORKSPACE_ID:?SLACRAWL_WORKSPACE_ID is required}"
channel_id="${SLACRAWL_CHANNEL_ID:?SLACRAWL_CHANNEL_ID is required}"
since_ts="${SLACRAWL_SINCE_TS:?SLACRAWL_SINCE_TS is required}"

go test ./...
go build -o "$tmpdir/slacrawl" ./cmd/slacrawl

"$tmpdir/slacrawl" --config "$tmpdir/config.toml" init --db "$tmpdir/slacrawl.db" >/dev/null

echo "== doctor =="
"$tmpdir/slacrawl" --json --config "$tmpdir/config.toml" doctor

echo "== desktop sync =="
"$tmpdir/slacrawl" --json --config "$tmpdir/config.toml" sync --source desktop

echo "== api sync =="
"$tmpdir/slacrawl" --json --config "$tmpdir/config.toml" sync \
  --source api \
  --workspace "$workspace_id" \
  --channels "$channel_id" \
  --since "$since_ts"

if [[ -n "${SLACK_APP_TOKEN:-}" ]]; then
  echo "== tail smoke =="
  "$tmpdir/slacrawl" --config "$tmpdir/config.toml" tail --workspace "$workspace_id" --repair-every 30m >/dev/null 2>/dev/null &
  tail_pid=$!
  sleep 3
  kill "$tail_pid" >/dev/null 2>&1 || true
  wait "$tail_pid" >/dev/null 2>&1 || true
fi

echo "== final doctor =="
"$tmpdir/slacrawl" --json --config "$tmpdir/config.toml" doctor

echo "== status =="
"$tmpdir/slacrawl" --json --config "$tmpdir/config.toml" status
