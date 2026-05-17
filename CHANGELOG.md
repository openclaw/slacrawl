# Changelog

## 0.6.1 - Unreleased

### Fixes

- Homebrew tap sync now skips cleanly when `HOMEBREW_TAP_GITHUB_TOKEN` is not configured.
- Add cached release checks with `slacrawl check-update` and passive terminal
  notices when a newer OpenClaw release is available.

## 0.6.0 - 2026-05-15

### Changes

- Added Slack export ZIP and directory import.
- Added user-token sync for DMs and MPIMs.
- Added the `analytics` command group.
- Clarified the Slack archive source model in CLI/reporting.
- Moved top-level CLI parsing and the `search`, `messages`, and `sql` read commands onto Kong while preserving existing output and config behavior.
- Added a local Docker image with `/data` persistence, Node support for desktop decoding, and CI smoke coverage.
- Added sqlc infrastructure and generated typed wrappers for stable store queries.
- Added Slack file metadata storage, `files`/`files fetch`, opt-in media caching, and git-share backup/restore for cached public-channel media.
- Documented Slack file media caching.

### Fixes

- Release workflow now calls the Homebrew tap sync path correctly.
- Stabilized analytics report clocks in tests and generated output.
- Kong helper parsing now preserves the intended top-level command behavior.
- Fixed Slack deleted-message events so live tail marks the original message row deleted instead of inserting a synthetic row at the event timestamp.
- Preserved archived reply and file metadata when live deleted-message events mark an existing message deleted.
- Refreshed message search text when live deleted-message events mark an existing message deleted.
- Handled Slack deleted-message payloads that omit `previous_message`.
- Indexed mentions when a live deleted-message event creates a tombstone row before the original message was archived.
- Socket Mode live tail now ACKs Slack events only after they are persisted.
- Slack links are parsed before entity decoding, and HTML entities are decoded once before indexing message search text and mentions.
- `search --help`, `messages --help`, and `sql --help` now print command help without loading config, and `search --limit N` supports bounded result sets.
- `analytics --help`, `analytics -h`, and `analytics help` now print analytics subcommand usage.
- `analytics quiet` and `analytics trends` now reject unexpected positional arguments instead of ignoring them.
- `make clean` now removes custom `BINARY` and `COMPLETION_DIR` outputs.
- Digest reports now exclude messages after the advertised `until` timestamp.
- Digest totals now count active authors per workspace when aggregating multiple workspaces.
- Message search indexing now includes visible Slack block and attachment text.
- Desktop IndexedDB ingest now indexes visible Slack block and attachment text.
- Share imports now validate manifest tables, shard paths, columns, and row counts before replacing snapshots.
- Share imports now reject manifest table directories that resolve outside the share repo.
- Git-share pulls now preserve local commits instead of resetting the branch to `origin`.
- API sync now skips unreadable thread replies instead of aborting the whole workspace sync.
- Slack export imports now reject cross-workspace channel/timestamp collisions instead of silently skipping or overwriting messages.
- Slack export imports now preserve leading and trailing whitespace in message text.
- Media fetch now validates every redirect target before sending Slack file requests.
- Slack export directory imports now reject traversal-style channel names before reading message files.
- Desktop draft ingest now preserves the workspace and user from Slack's local draft keys.
- Desktop ingest now removes temporary Slack snapshot copies after use and after snapshot setup errors.
- Config normalization now trims explicit `workspace_id` values before workspace lookups.
- Read-only SQL now rejects writable CTEs and extra statements before executing queries.
- Store writes now reject cross-workspace channel, user, and message key collisions instead of overwriting the existing workspace row.
- Older store databases now run ordered migrations before updating SQLite `user_version`.
- Message filters now stay indexable on workspace, channel, user, and timestamp read paths.

### Maintenance

- Updated Go dependencies and lint rules, including the `golang.org/x/text` security bump.
- Added issue and pull request auto-assignment workflow coverage.
- Refreshed slacrawl skill documentation and usage notes.

## 0.5.0 - 2026-04-22

### Changes

- Added `digest` for windowed per-channel activity summaries.
- Expanded README coverage for git-share usage and v0.5.0 install snippets.

### Fixes

- Upgraded GitHub Actions usage for Node 24 compatibility.

## 0.4.0 - 2026-04-22

### Changes

- Added git-backed archive sync workflow.
- Hardened indexed text and added read-path indexes.
- Added archive report and share freshness views.
- Updated release/install documentation for v0.4.0.

### Fixes

- Release automation now rebases tap sync changes before pushing.

## 0.3.2 - 2026-04-16

### Fixes

- Published packages to the fixed Cloudsmith repository.
- Retargeted the Homebrew tap for v0.3.2.

## 0.3.1 - 2026-03-14

### Fixes

- Populated channel IDs on messages returned from `conversations.history` and `conversations.replies`.
- Added regression coverage for missing channel IDs in Slack API sync.

### Documentation

- Refreshed README content after v0.3.0.

## 0.3.0 - 2026-03-08

### Changes

- Refreshed CLI presentation and shell completion.
- Added ergonomic multi-workspace sync and live tail support.
- Added configuration documentation and repo hygiene files.

### Documentation

- Sharpened product positioning and install documentation.
- Refreshed README and spec coverage for the new multi-workspace flow.

## 0.2.0 - 2026-03-08

### Documentation

- Updated README release/install wording for v0.2.0.

## 0.1.0 - 2026-03-08

### Changes

- Bootstrapped the slacrawl CLI and SQLite sync core.
- Added post-bootstrap sync updates.
- Added release automation and packaging workflows.

### Documentation

- Defined the slacrawl product contract.
- Refreshed README and contributor documentation for the initial release.
