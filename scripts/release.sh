#!/usr/bin/env bash
set -euo pipefail

echo "local releases are disabled because this path cannot guarantee notarized macOS artifacts" >&2
echo "official releases must use: gh workflow run release-unified.yml --repo openclaw/slacrawl -f version=X.Y.Z" >&2
exit 1
