# slacrawl Spec

This file is the build contract for contributors working in this repo.

Goal:

- build a local-first Slack crawler
- mirror Slack workspace data the configured app can access
- store it in SQLite
- support fast text search and raw SQL
- support one-shot backfill and, where credentials allow, live sync

## Product Summary

`slacrawl` is a Go CLI that mirrors Slack workspace data into local SQLite.

V1 scope:

- multi-workspace storage
- one or many workspaces in CLI sync and tail when explicitly configured
- public channels
- private channels
- top-level messages
- channel threads
- current workspace user snapshot
- FTS5 search
- raw SQL access
- desktop-local Slack discovery on macOS and Linux
- external archive ingestion through a local JSONL provider protocol

Out of scope for V1:

- attachment blob downloads by default
- write-back actions
- Marketplace/public-distribution hardening

## Requirements Already Chosen

- config format: `TOML`
- config location: `~/.slacrawl/config.toml`
- DB location: `~/.slacrawl/slacrawl.db`
- cache dir: `~/.slacrawl/cache/`
- log dir: `~/.slacrawl/logs/`
- language: Go
- schema: single-workspace default, multi-workspace-ready
- search: FTS5 first, embeddings later
- source precedence: user-token API, then bot-token API and slack-export imports, then desktop-local cache; external providers must use a numeric rank greater than `2`, and equal ranks may replace
- files: metadata only in DB for V1
- future file-blob backup must store Git-share media as gzip-compressed files, import those files back into raw local cache layout, and keep legacy raw-media import compatibility
- desktop-local source: supported Slack Desktop cache paths on macOS and Linux

## Local Environment Contract

An agent should assume:

- shell: `zsh`
- Go `1.26.5+` is installed
- desktop-local Slack data may exist under:
  - `~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack`
  - `${XDG_CONFIG_HOME}/Slack`
  - `~/.config/Slack`

## Slack Data Model Notes

Important Slack facts that drive the schema:

- messages are scoped by `(channel_id, ts)`
- threads remain message relationships via `thread_ts`
- historical thread replies for public/private channel threads require a user token
- live updates should use Socket Mode when enabled
- desktop-local data is an optional read-only source and must never become a write path

## Database Design

Use SQLite with:

- WAL mode
- foreign keys on
- FTS5 enabled

Tables:

- `workspaces`
- `channels`
- `users`
- `messages`
- `message_events`
- `sync_state`
- `message_mentions`
- `embedding_jobs`
- `message_fts`

`channels.kind` values include:

- `public_channel`
- `private_channel`
- `public`
- `private`
- `im`
- `mpim`

Optional later:

- `message_embeddings`

## Search Design

V1 search mode is `fts`.

Normalize:

- Slack mrkdwn
- user mentions
- channel references
- URLs
- file titles
- thread context
- edited and deleted markers

## CLI Spec

Usage:

```text
slacrawl [global flags] <command> [args]
```

Commands:

- `init`
- `doctor`
- `publish`
- `subscribe`
- `update`
- `sync`
- `import`
- `purge`
- `tail`
- `watch`
- `search`
- `messages`
- `mentions`
- `sql`
- `users`
- `channels`
- `status`
- `report`
- `digest`
- `analytics`

### `sync`

Purpose:

- one-shot crawl

Expected flags:

- `--source api|bot|desktop|wiretap|mcp|connector|all|provider:<name>`
- `--workspace <id>`
- `--channels <csv>`
- `--exclude-channels <csv>`
- `--since <timestamp>`
- `--full`
- `--latest-only`
- `--limit <messages>` for bounded external-provider validation imports
- `--concurrency <n>`
- `--auto-join=<bool>`

### `doctor`

Must check:

- config file readability
- token presence and shape
- DB openability
- FTS presence
- desktop-local source availability
- whether thread coverage can be full or only partial
- if a configured user token actually auths successfully
- recent API channel skips and tail connection/repair state when present
- configured git-share repo plus last import / stale state when share mode is enabled

### `purge`

Purpose:

- enforce local message-retention cutoffs
- preview destructive impact before changing the archive

Expected flags:

- exactly one of `--before <RFC3339|YYYY-MM-DD>` or `--older-than <duration>`
- optional `--workspace <id>`
- `--force` to execute; omission is a preview
- `--keep-media` to retain cached media no longer referenced by stored messages
- optional `--keep-message-events <n>` to retain the newest events per message, event type, and source
- `--vacuum` to compact SQLite after deletion

Behavior:

- cutoff is exclusive
- thread retention uses the parent timestamp, deleting an expired parent and all replies together
- desktop drafts use their own encoded timestamp or update time, even when attached to an expired thread
- delete messages and message-owned events, file metadata, mentions, embedding jobs, and FTS rows in one transaction
- preserve workspaces, channels, users, and sync state
- record per-channel retention floors so incremental API/MCP repair overlap does not restore purged history
- delete only cached media paths with no remaining database references
- preview and compact retained event history only when `--keep-message-events` is provided
- do not compact the SQLite file unless `--vacuum` is set

### `status`

Must include:

- workspace, channel, user, message, and mention totals
- sync metadata such as first / last timestamps
- configured git-share repo plus last import / stale state when share mode is enabled

### `users`

Purpose:

- list synced users, optionally filtered by workspace and a positional text query
- return up to 100 rows by default, with `--limit <n>` accepting positive overrides

### `channels`

Purpose:

- list synced channels, optionally filtered by workspace, channel kind, and a positional text query
- return up to 100 rows by default, with `--limit <n>` accepting positive overrides

### `report`

Purpose:

- summarize archive activity without writing SQL

Must include:

- total messages plus draft / edited / deleted counts
- bounded windows for recent message activity
- top channels, authors, and busiest days
- git-share freshness state when share mode is enabled

### `digest`

Purpose:

- windowed per-channel activity summary derived from the local store

Expected flags:

- `--since <duration>` lookback window, accepts Go durations (`72h`) or day shorthand (`7d`, `30d`). Default: `7d`.
- `--workspace <id>`
- `--channel <id-or-name>`
- `--top-n <int>` top posters and top mention targets per channel. Default: `1`.

Must include:

- per-channel message count, thread count (parent messages with replies), and active-author count
- top posters per channel (respects `--top-n`)
- top mention targets per channel (respects `--top-n`)
- window totals: messages, threads, channels, active authors

### `analytics`

Purpose:

- grouped analytics subcommands derived from local store data

Subcommands in this phase:

- `analytics digest [--since 7d] [--workspace <id>] [--channel <id-or-name>]`
- `analytics quiet [--since 30d] [--workspace <id>]`
- `analytics trends [--weeks 8] [--workspace <id>] [--channel <id-or-name>]`

### `tail`

Purpose:

- live sync from Socket Mode

Requirements:

- app-level token required
- reconnect automatically
- write checkpoints
- periodic incremental repair sync

### `watch`

Purpose:

- periodic desktop-local refresh loop

Requirements:

- desktop source must be enabled
- interval defaults from config
- `--workspace <id>` optionally restricts refreshes to one desktop workspace
- omission of `--workspace` refreshes every workspace in the signed-in desktop profile
- append/upsert into the existing DB

## Config Spec

Format:

- TOML

Location:

- `~/.slacrawl/config.toml`

Credential model:

- bot token: `xoxb-`
- app token: `xapp-`
- optional user token: `xoxp-`
- each token source can be enabled or disabled independently
- desktop source can be enabled or disabled independently
- blank desktop path means auto-detect the supported macOS or Linux Slack path
- optional `[[workspaces]]` entries can override bot/app/user token env vars per workspace
- workspace token lookup should default to `SLACK_<WORKSPACE_ID>_BOT_TOKEN`, `SLACK_<WORKSPACE_ID>_APP_TOKEN`, and `SLACK_<WORKSPACE_ID>_USER_TOKEN`
- `[sync].auto_join` defaults to `true` and controls whether API sync attempts to join public channels before retrying history
- `[sync].exclude_channels` is an optional case-insensitive list of channel names to skip during API sync and merges with `--exclude-channels`

External provider config:

- each `[[providers]]` entry has a unique lowercase `name` without whitespace, slashes, or colons
- `command` is required, expands `~` or a leading `~/`, and must resolve to an absolute path
- `args` are passed directly to the command without a shell
- `env_allowlist` names additional environment variables forwarded alongside the minimal runtime environment
- `source_rank` is required and must be greater than `2`; lower numeric ranks win during message reconciliation, while equal ranks may replace
- `batch_size` defaults to `1000`, must be between `1` and `100000`, and is forwarded as an upstream batching hint

Share config:

- `[share].remote` points at the git remote that stores compressed archive snapshots
- `[share].repo_path` is the local clone / working repo path used for publish and update
- `[share].branch` defaults to `main`
- `[share].auto_update` controls whether read commands import stale git snapshots before querying
- `publish --tag <name>` creates an immutable tag for a committed snapshot
- routine `update` imports merge by stable row identity, preserve destination-only rows and newer tombstones, and never infer deletion from a row missing in the snapshot
- `update --restore` is the explicit exact-replacement mode
- `update --restore --ref <tag-or-commit>` restores a historical snapshot without changing the share checkout
- file and mention rows retain `deleted_at`, `deletion_source`, and `deletion_reason` tombstones when an authoritative message payload or parent-delete event removes them
- `[share].stale_after` defines how old the last successful import can be before auto-refresh runs
- share sync state should record both the last successful import time and the last imported manifest generation time

## Sync Algorithm

### API sync

1. load config
2. resolve tokens
3. auth test
4. fetch workspace metadata
5. fetch channels
6. derive per-channel sync window:
   - explicit `--since` wins
   - `--full` disables incremental cutoffs
   - `--latest-only` skips channels that do not already have a stored cursor
   - otherwise reuse the latest stored per-channel timestamp with overlap
7. apply any configured or CLI-provided excluded channel-name filters after channel discovery and allow-list filtering
8. fetch users
9. backfill message history
10. when `auto_join` is enabled, attempt public-channel join and retry once on `not_in_channel`
11. backfill thread replies only when a user token is configured and successfully auths
12. normalize messages
   - repair malformed UTF-8 before indexing
   - normalize indexed text with NFKC
   - strip zero-width and non-printable control noise
   - collapse odd whitespace for stable FTS / mention extraction
13. upsert canonical rows
14. update FTS rows and mentions
15. write checkpoints, channel skips, and join attempts

### External provider sync

1. resolve `provider:<name>` against `[[providers]]` and require a workspace ID
2. choose a checkpoint key from the provider name, workspace, and normalized invocation scope
   - the unfiltered incremental run uses the workspace checkpoint
   - `--since`, `--full`, `--latest-only`, channel filters, exclusions, and `--limit` use isolated scope checkpoints
3. start the absolute command directly with configured args and a minimal environment plus `env_allowlist`
4. send one `slacrawl-provider-v1` JSON request on stdin containing `workspace_id`, `since`, `full`, `latest_only`, channel filters, saved opaque `checkpoint`, `batch_size`, and optional positive `limit`
5. consume JSONL from stdout
   - the first record must be `hello` with the matching protocol
   - data records may be `workspace`, `channel`, `user`, or `message`
   - `checkpoint` records must identify the requested workspace and contain a nonempty opaque value
   - the terminal `done.records` count must equal the number of data records consumed; checkpoints are not counted
6. reject cross-workspace records, missing required identities or message channels, records after `done`, and more messages than `limit`
7. build normalized message search text, extract mentions, update FTS, and preserve existing messages with a lower numeric source rank
8. when a checkpoint is present, atomically commit it with the pending record batch; committed batches and checkpoints remain resumable if the process later fails
9. require `done` plus a zero exit status for overall success
10. after a successful unbounded `--full`, promote the final full checkpoint to the matching incremental scope

Provider v1 response records:

- `hello`: `type`, `protocol`, optional provider implementation name
- `workspace`: `type`, requested workspace `id`, optional metadata and `raw_json`
- `channel`: `type`, `workspace_id`, `id`, optional metadata and `raw_json`
- `user`: `type`, `workspace_id`, `id`, optional profile data and `raw_json`
- `message`: `type`, `workspace_id`, `channel_id`, Slack `ts`, optional `user_id`, `thread_ts`, `text`, and `raw_json`
- `checkpoint`: `type`, `entity_type = "workspace"`, requested workspace `entity_id`, and opaque nonempty `value`
- `done`: `type`, data-record count in `records`, plus optional provider quality counters

The provider owns the interpretation of `since`, `full`, `latest_only`,
`channels`, and `exclude_channels`; the consumer enforces workspace ownership
and the positive message limit. A message channel must already exist locally or
be emitted earlier in the stream. Unknown nonempty message user IDs are reserved
as sparse workspace-bound profiles that a later real user record can enrich.
Incremental imports enforce stored retention floors; `--full` or an explicit
`--since` older than the floor is a deliberate restore that may reintroduce
purged history.

### Git share sync

1. clone or open the configured share repo
2. read `manifest.json`
3. skip import when the manifest generation timestamp matches the last imported manifest
4. otherwise clear canonical tables and import the sharded compressed JSONL snapshot
5. rebuild FTS rows locally
6. record last import timestamps in `sync_state`
7. future file/media blobs must be exported as gzip-compressed share files, restored to raw local cache files during import, and keep legacy raw-media import compatibility

### Desktop-local sync

1. discover the Slack Desktop path
2. snapshot/copy source artifacts before parsing
3. parse `storage/root-state.json`
4. inspect IndexedDB and Local Storage artifacts
5. ingest supported desktop-local metadata:
   - workspace/user metadata from `localConfig_v2`
   - cached channel metadata, member profiles, and channel message history from IndexedDB redux persistence blobs when `node` is available
   - cached thread roots and cached reply messages from IndexedDB redux persistence blobs when present
   - draft bodies and thread draft destinations
   - recent-channel hints
   - `conversations.mark` read markers
   - custom-status state
   - IndexedDB object store inventory for drift detection

## Recommended Go Package Layout

```text
cmd/slacrawl/
internal/cli/
internal/config/
internal/provider/
internal/share/
internal/slackapi/
internal/slackdesktop/
internal/store/
internal/search/
internal/syncer/
internal/embed/
```

## Milestones

### Milestone 0

- spec and contributor docs
- schema contract
- desktop reverse-engineering fixture plan

### Milestone 1

- config loader
- `init`
- `doctor`
- `status`
- DB open + migrations

### Milestone 2

- workspace metadata sync
- channel sync
- user sync
- message backfill
- FTS indexing

### Milestone 3

- thread coverage
- search
- sql
- users
- channels
- messages
- mentions

### Milestone 4

- desktop-local adapter hardening
- source reconciliation

### Milestone 5

- `tail`
- reconnect logic
- repair loop

## What The Repo Must Eventually Contain

- this spec
- README
- CONTRIBUTING guide
- config sample
- schema and migration files
- CLI contract in code
- tests for config, search, API sync, and desktop-local parsing
