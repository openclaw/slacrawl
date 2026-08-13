#!/usr/bin/env bash
set -euo pipefail

echo "local releases are disabled; the unified workflow owns tags, signing, notarization, and publication" >&2
echo "official releases must use: gh workflow run release-unified.yml --repo openclaw/slacrawl -f version=X.Y.Z" >&2
exit 1
