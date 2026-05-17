<h1 align="center">💾 slacrawl</h1>

<p align="center">
  <strong>A Go-based CLI for mirroring Slack workspace data into local SQLite<br> for search, querying, and offline inspection.</strong>
</p>

<p align="center">
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/vincentkoc/slacrawl" alt="License"></a>
  <img src="https://img.shields.io/badge/go-1.26.3%2B-00ADD8" alt="Go 1.26.3+">
  <img src="https://img.shields.io/badge/storage-SQLite-003B57" alt="SQLite">
  <img src="https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey" alt="Platform">
</p>

<p align="center">
  <img src="screenshot.png" alt="slascrawl Demo"/>
</p>

## Why slacrawl?

Slack search is convenient until you need your own workflow, your own retention, or your own queries. `slacrawl` is a Go-based CLI that pulls Slack workspace metadata and message history into SQLite so you can inspect it without depending on the Slack UI.

Data stays on your machine. You can run it in API mode, desktop mode, or a hybrid workflow that combines both. That covers one-shot syncs, live tailing over Socket Mode, and local desktop recovery or "wiretap" style inspection from Slack Desktop artifacts already on your machine.

## Included

- local SQLite storage with full-text search backed by SQLite FTS5
- workspace, channel, user, and message sync
- thread reply backfill when a user token is available
- DM and MPIM sync when a user token is available
- incremental API history sync by default, with `--full` reserved for deliberate backfills
- `sync --latest-only` for cheap incremental refreshes on already-seeded channels
- mention extraction for structured querying
- read-only SQL access for ad hoc analysis
- `doctor` diagnostics for config, database, token, and desktop-source checks
- desktop-local ingestion of workspace metadata, channels, users, cached channel messages, drafts, read markers, recent-channel hints, and custom-status metadata
- optional Socket Mode live tailing via app token
- periodic desktop refresh with `watch`
- git-backed archive publishing, subscription, and read-time auto-refresh

## Current Coverage

- multi-workspace storage and filtering
- multi-workspace API sync when `[[workspaces]]` is configured
- multi-workspace live tailing when per-workspace app tokens are configured
- public channels
- private channels
- top-level messages
- channel threads
- local FTS search
- read-only SQL access
- macOS Slack Desktop discovery
- optional Slack file media caching

## Not Yet Included

- write-back actions
- public Marketplace-style distribution hardening
- desktop-local message extraction beyond the documented bootstrap surface

If one of those gaps matters to your workflow, open an issue so it can be tracked explicitly.

## Requirements

- Go `1.26.3+`
- `node` if you want richer desktop-local IndexedDB blob decoding
- a Slack bot token for standard API sync
- an app token if you want to use `tail`
- an optional user token for fuller historical thread coverage
- macOS Slack Desktop only if you want desktop-local discovery

## Install

<details open>
<summary>Homebrew (macOS)</summary>

```bash
brew tap vincentkoc/tap
brew install slacrawl
```

</details>

<details>
<summary>Linux packages from GitHub Releases</summary>

Download the package that matches your platform from the [latest release](https://github.com/openclaw/slacrawl/releases/latest).

Debian/Ubuntu:

```bash
curl -LO https://github.com/openclaw/slacrawl/releases/latest/download/slacrawl_0.6.0_amd64.deb
sudo dpkg -i slacrawl_0.6.0_amd64.deb
```

RHEL/Fedora:

```bash
curl -LO https://github.com/openclaw/slacrawl/releases/latest/download/slacrawl-0.6.0-1.x86_64.rpm
sudo rpm -i slacrawl-0.6.0-1.x86_64.rpm
```

</details>

<details>
<summary>Build from source</summary>

```bash
git clone https://github.com/openclaw/slacrawl.git
cd slacrawl
go build -o bin/slacrawl ./cmd/slacrawl
./bin/slacrawl --help
```

</details>

<details>
<summary>Docker</summary>

```bash
docker build -t slacrawl .
docker run --rm -e SLACK_BOT_TOKEN -v "$PWD/.slacrawl:/data" slacrawl doctor
docker run --rm -e SLACK_BOT_TOKEN -e SLACK_APP_TOKEN -v "$PWD/.slacrawl:/data" slacrawl tail
```

The image stores config, SQLite data, cache, and Git snapshot state under `/data`. Desktop/wiretap mode needs an explicit host mount to the Slack Desktop data directory.

</details>

<details>
<summary>Run without building a binary</summary>

```bash
git clone https://github.com/openclaw/slacrawl.git
cd slacrawl
go run ./cmd/slacrawl --help
```

</details>

## Quick Start

```bash
export SLACK_BOT_TOKEN="xoxb-..."
export SLACK_APP_TOKEN="xapp-..."
export SLACK_USER_TOKEN="xoxp-..."

go run ./cmd/slacrawl init
go run ./cmd/slacrawl doctor
go run ./cmd/slacrawl sync --source bot
go run ./cmd/slacrawl search --workspace T01234567 "incident"
go run ./cmd/slacrawl analytics trends --weeks 4
go run ./cmd/slacrawl tail --repair-every 30m
go run ./cmd/slacrawl watch --desktop-every 5m
```

If you built the binary, replace `go run ./cmd/slacrawl` with `./bin/slacrawl`.

`tail` is the live API side of the tool. `watch` is the recurring desktop-side refresh loop.

## Visibility Model

Treat one `slacrawl` config/database as one Slack visibility boundary. The archive should mean "what this bot/account/profile can see", not a blend of unrelated personal and company backups.

- `--source bot` is an alias for `--source api`; it crawls Slack through configured bot/user tokens
- `--source wiretap` is an alias for `--source desktop`; it reads the local Slack Desktop cache
- `--source all` runs API first, then desktop enrichment
- `[share]` is a backup/restore target for the current DB, not a second Slack source

For separate company and personal archives, use separate configs with separate `db_path` and `[share].remote` values.

Choose the path that matches your setup:

- use `sync --source bot` for normal token-backed incremental syncs
- use `sync --source bot --full` only when you want a deliberate full backfill
- use `sync --source bot --latest-only` when you only want fresh deltas on channels that already have local history
- use `sync --source wiretap` when you want local desktop recovery only
- use `watch` when you want desktop-local state to refresh into SQLite continuously

## Commands

- `init` creates a starter config file
- `doctor` checks config, DB access, token presence, FTS, and desktop source availability
- `report` summarizes archive activity and git-share freshness without writing SQL
- `publish` exports the local SQLite archive into a git repo as compressed JSONL shards plus a manifest
- `subscribe` configures a git-backed reader that can run without Slack credentials
- `update` pulls and imports the latest git snapshot
- `sync` performs a one-shot crawl from bot/API, wiretap/desktop, or both
- `import` imports a Slack export ZIP or extracted export directory
- `tail` listens for live events through Socket Mode, including one tail per configured workspace
- `watch` refreshes desktop-local state on a schedule
- `search` runs local FTS queries, optionally filtered by workspace
- `messages` lists stored messages with filters
- `mentions` lists structured mention records
- `sql` runs read-only SQL against the local database
- `users` lists synced users
- `channels` lists synced channels
- `status` prints workspace and sync status
- `metadata --json`, `status --json`, and `doctor --json` expose crawlkit
  control/status payloads for launchers, automation, and CI
- `check-update` checks GitHub Releases for newer OpenClaw builds; interactive
  terminal runs also print a cached daily stderr notice unless
  `SLACRAWL_NO_UPDATE_CHECK=1` or `CRAWLKIT_NO_UPDATE_CHECK=1` is set
- `digest` prints a per-channel activity summary for a time window
- `analytics` groups analytics subcommands (`digest`, `quiet`, `trends`)
- `completion` prints shell completion for `bash` or `zsh`

## Importing a Slack Export

```bash
slacrawl import ./my-export.zip --workspace T01234567
slacrawl import ./extracted-export/ --workspace T01234567 --dry-run
```

Set `SLACK_USER_TOKEN` with `im:history`, `mpim:history`, `im:read`, and `mpim:read` scopes to include DMs and MPIMs in API sync.

## Analytics

- `analytics digest [--since 7d] [--workspace X] [--channel C]`
- `analytics quiet [--since 30d] [--workspace X]`
- `analytics trends [--weeks 8] [--workspace X] [--channel C]`

Planned follow-ups: `health`, `response-times`, `threads-stale`, `activity`.

## Output Modes

The CLI supports three output modes:

- `--format text` for the styled default terminal view
- `--format json` or `--json` for machine-readable output
- `--format log` for line-oriented automation-friendly output

Color is disabled automatically when stdout is not a TTY. You can also force plain text with `--no-color` or `NO_COLOR=1`.

## Make Targets

```bash
make build
make test
make run ARGS="status"
make generate-sqlc
make completion
```

Completion files are generated into `dist/completions/`.

## Shell Completion

Generate completion scripts with:

```bash
go run ./cmd/slacrawl completion bash
go run ./cmd/slacrawl completion zsh
```

Or use the Makefile:

```bash
make completion
```

Typical install locations:

```bash
# bash
go run ./cmd/slacrawl completion bash > /usr/local/etc/bash_completion.d/slacrawl

# zsh
mkdir -p "${HOME}/.zsh/completions"
go run ./cmd/slacrawl completion zsh > "${HOME}/.zsh/completions/_slacrawl"
```

## Default Paths

- config: `~/.slacrawl/config.toml`
- database: `~/.slacrawl/slacrawl.db`
- cache: `~/.slacrawl/cache`
- logs: `~/.slacrawl/logs`

## Configuration

For one workspace, keep using the top-level `[slack.bot]`, `[slack.app]`, and `[slack.user]` token config.

For multiple API workspaces or multiple live wiretap/tail sessions, add `[[workspaces]]` entries with per-workspace token env vars:

```toml
workspace_id = "T01234567"

[[workspaces]]
id = "T01234567"
default = true

[[workspaces]]
id = "T08976543"
bot_token_env = "SLACK_CLIENT_BOT_TOKEN"
app_token_env = "SLACK_CLIENT_APP_TOKEN"
user_token_env = "SLACK_CLIENT_USER_TOKEN"
```

By default, each workspace entry automatically looks for `SLACK_<WORKSPACE_ID>_BOT_TOKEN`, `SLACK_<WORKSPACE_ID>_APP_TOKEN`, and `SLACK_<WORKSPACE_ID>_USER_TOKEN`, so you only need the `id` in the common case. Top-level `enabled` flags still apply globally, which avoids repeating `enabled = true` per workspace.

Without `--workspace`, `sync --source bot` and `tail` fan out across every configured workspace entry. Read commands such as `search`, `messages`, `mentions`, `users`, and `channels` accept `--workspace` to scope the shared local database when needed.

File media downloads are opt-in. SQLite stores file metadata and cache pointers; bytes live under `cache_dir/media`.

```toml
[sync]
file_media = false
max_file_bytes = 104857600
```

Use `sync --with-media` or `files fetch` when you want to populate the local cache.

```bash
go run ./cmd/slacrawl files --filename runbook
go run ./cmd/slacrawl files fetch --missing --max-bytes 104857600
go run ./cmd/slacrawl sync --source bot --latest-only --with-media
```

## Git Archive Sharing

Use git-share mode when one machine has Slack credentials and should publish snapshots, while other machines only need a local read-only archive.

Typical split:

- publisher machine: runs `sync`, then `publish --push`
- subscriber machine: runs `subscribe`, then reads from local SQLite with optional read-time auto-refresh

Git-backed archive sharing is configured under `[share]`:

```toml
[share]
remote = "git@github.com:your-org/private-slacrawl-archive.git"
repo_path = "~/.slacrawl/share"
branch = "main"
auto_update = true
stale_after = "15m"
```

Behavior:

- `publish` writes gzipped JSONL shards plus `manifest.json` into `repo_path`
- cached non-DM/non-private file media is included by default; use `--no-media` to omit it
- `subscribe` writes a git-reader config, disables Slack API and desktop sources for that config, clones the repo, and imports the snapshot
- pass `--db` to `subscribe` when you want the reader archive to land in a non-default SQLite path
- `update` pulls and re-imports only when the manifest changes
- `status`, `search`, `messages`, `mentions`, `sql`, `users`, `channels`, and `report` auto-refresh stale git snapshots before reading when `auto_update = true`
- `sync --source bot` and `sync --source all` warm from the git snapshot before hitting Slack when a share remote is configured
- `status` and `doctor` surface the current git-share repo, last import time, and whether the local snapshot is stale

### `publish`

`publish` is the writer-side command. It exports the current SQLite archive into the git share repo and can commit/push it in one step.

```bash
go run ./cmd/slacrawl publish --remote /path/to/private/slacrawl-archive.git --push
go run ./cmd/slacrawl publish --repo ~/.slacrawl/share --branch main --message "archive: daily refresh" --push
```

Relevant flags:

- `--repo` chooses the local git working repo path
- `--remote` sets or overrides the git remote used for publish
- `--branch` chooses the target branch
- `--message` sets the git commit message
- `--no-commit` exports files without creating a git commit
- `--push` pushes the new commit to `origin`
- `--no-media` omits cached media files from the snapshot

### `subscribe`

`subscribe` is the reader-side setup command. It clones the git archive, writes a share-reader config, disables live Slack sources for that config, and imports the snapshot into SQLite.

```bash
go run ./cmd/slacrawl subscribe --repo ~/.slacrawl/share --db ~/.slacrawl/slacrawl.db /path/to/private/slacrawl-archive.git
go run ./cmd/slacrawl subscribe --remote git@github.com:your-org/private-slacrawl-archive.git --stale-after 30m
go run ./cmd/slacrawl subscribe --repo ~/.slacrawl/share --no-import --no-auto-update /path/to/private/slacrawl-archive.git
```

Relevant flags:

- `--repo` chooses the local clone path
- `--db` chooses the SQLite file used by the reader
- `--branch` chooses which branch to track
- `--remote` stores the remote in config without requiring it as a positional arg
- `--stale-after` controls when read-time refresh considers the local snapshot stale
- `--no-auto-update` disables read-time refresh for search/status/report-style commands
- `--no-import` skips the initial snapshot import
- `--no-media` skips restoring cached media files

### `update`

`update` is the explicit reader-side refresh. Use it when you want to pull and import on demand instead of waiting for automatic stale checks.

```bash
go run ./cmd/slacrawl update
go run ./cmd/slacrawl update --repo ~/.slacrawl/share --branch main
```

### `report`

`report` is the fastest human-readable archive summary and is especially handy in git-share mode because it shows the current archive footprint plus share freshness.

```bash
go run ./cmd/slacrawl report
```

Typical publish / subscribe flow:

```bash
# publisher
go run ./cmd/slacrawl sync --source bot --latest-only
go run ./cmd/slacrawl publish --remote /path/to/private/slacrawl-archive.git --push

# subscriber
go run ./cmd/slacrawl subscribe --repo ~/.slacrawl/share --db ~/.slacrawl/slacrawl.db /path/to/private/slacrawl-archive.git
go run ./cmd/slacrawl search incident
```

The starter config lives in [`config.example.toml`](./config.example.toml). By default it points to these environment variables:

- `SLACK_BOT_TOKEN`
- `SLACK_APP_TOKEN`
- `SLACK_USER_TOKEN`

Desktop discovery is enabled by default and uses:

```text
~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack
```

Desktop config notes:

- set `[slack.desktop].enabled = false` to disable desktop ingestion
- leave `[slack.desktop].path = ""` to auto-detect the macOS Slack path
- set a custom absolute path if Slack Desktop data lives elsewhere
- set `[slack.bot]`, `[slack.app]`, or `[slack.user]` `enabled = false` to ignore that token source entirely

Separate visibility profiles should use separate config files:

```toml
# ~/.slacrawl/company.toml
db_path = "~/.slacrawl/company.db"

[share]
remote = "git@github.com:your-org/company-slacrawl-archive.git"
repo_path = "~/.slacrawl/company-share"
```

```toml
# ~/.slacrawl/personal.toml
db_path = "~/.slacrawl/personal.db"

[share]
remote = "git@github.com:your-user/personal-slacrawl-archive.git"
repo_path = "~/.slacrawl/personal-share"
```

## Typical Workflow

```bash
go run ./cmd/slacrawl init
go run ./cmd/slacrawl sync --source bot
go run ./cmd/slacrawl status
go run ./cmd/slacrawl report
go run ./cmd/slacrawl digest --since 7d
go run ./cmd/slacrawl channels
go run ./cmd/slacrawl messages --channel C12345678 --limit 20
go run ./cmd/slacrawl mentions --limit 20
go run ./cmd/slacrawl sql 'select channel_id, count(*) as messages from messages group by channel_id order by messages desc limit 10;'
```

## Notes on Coverage

- Full historical thread reply coverage in public and private channels depends on providing a user token.
- `tail` requires an app token because it uses Socket Mode.
- SQLite FTS5 is the built-in full-text index that powers fast local text search without an external search server.
- Indexed text is sanitized before it reaches FTS, so malformed UTF-8, zero-width junk, and odd whitespace do not poison search.
- Desktop-local support is broader than simple discovery, but still not a full write-back or export-import path.

## Development

```bash
go test ./...
go build ./cmd/slacrawl
```

See [`CONTRIBUTING.md`](./CONTRIBUTING.md) for contribution workflow and [`SPEC.md`](./SPEC.md) for the implementation contract.

Deep-dive docs:

- [`docs/configuration.md`](./docs/configuration.md)
- [`docs/desktop-mode.md`](./docs/desktop-mode.md)

---

Built by <a href="https://github.com/vincentkoc">Vincent Koc</a> · <a href="./LICENSE">MIT</a>
