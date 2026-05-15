# Changelog

## 0.5.1 - Unreleased

### Changes

- Docker: add a local image with `/data` persistence, Node support for desktop decoding, and CI smoke coverage.
- Added Slack file metadata storage, `files`/`files fetch`, opt-in media caching, and git-share backup/restore for cached public-channel media.
- Moved top-level CLI parsing and the `search`, `messages`, and `sql` read commands onto Kong while preserving existing output and config behavior.

### Fixes

- `search --help`, `messages --help`, and `sql --help` now print command help without loading config, and `search --limit N` supports bounded result sets.
