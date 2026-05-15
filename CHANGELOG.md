# Changelog

## 0.5.1 - Unreleased

### Changes

- Docker: add a local image with `/data` persistence, Node support for desktop decoding, and CI smoke coverage.
- Added Slack file metadata storage, `files`/`files fetch`, opt-in media caching, and git-share backup/restore for cached public-channel media.
- Moved top-level CLI parsing and the `search`, `messages`, and `sql` read commands onto Kong while preserving existing output and config behavior.

### Fixes

- Fixed Slack deleted-message events so live tail marks the original message row deleted instead of inserting a synthetic row at the event timestamp.
- Handled Slack deleted-message payloads that omit `previous_message`.
- `analytics --help`, `analytics -h`, and `analytics help` now print analytics subcommand usage.
- `analytics quiet` and `analytics trends` now reject unexpected positional arguments instead of ignoring them.
- Digest reports now exclude messages after the advertised `until` timestamp.
- Digest totals now count active authors per workspace when aggregating multiple workspaces.
- Message search indexing now includes visible Slack block and attachment text.
- Share imports now validate manifest tables, shard paths, columns, and row counts before replacing snapshots.
- `make clean` now removes custom `BINARY` and `COMPLETION_DIR` outputs.
- Indexed mentions when a live deleted-message event creates a tombstone row before the original message was archived.
- Preserved archived reply and file metadata when live deleted-message events mark an existing message deleted.
- Refreshed message search text when live deleted-message events mark an existing message deleted.
- Slack HTML entities are now decoded before indexing message search text and mentions.
- Socket Mode live tail now ACKs Slack events only after they are persisted.
- `search --help`, `messages --help`, and `sql --help` now print command help without loading config, and `search --limit N` supports bounded result sets.
