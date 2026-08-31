# Command reference

`slacrawl` uses global flags before the command:

```text
slacrawl [--config <path>] [--format text|json|log] [--no-color] <command> [args]
```

`--json` is an alias for `--format json`. Run `slacrawl <command> --help` for the complete, current flag list.

## Setup and diagnostics

| Command | Purpose |
| --- | --- |
| `init` | Create a starter TOML config and set the SQLite database path. |
| `doctor` | Check config, database access, FTS5, token presence, desktop sources, and git-share freshness. |
| `status` | Show workspace, archive, sync, and git-share status. |
| `metadata` | Print crawlkit control metadata for launchers and automation. |
| `check-update` | Check GitHub Releases for a newer build. |

The default paths are:

| Data | Path |
| --- | --- |
| Config | `~/.slacrawl/config.toml` |
| SQLite database | `~/.slacrawl/slacrawl.db` |
| Cache | `~/.slacrawl/cache` |
| Logs | `~/.slacrawl/logs` |

Override the config with the global `--config <path>` flag. Database, cache, and log paths live in the TOML config; see [Configuration](configuration.md).

## Ingest and refresh

| Command | Purpose |
| --- | --- |
| `sync` | Run a one-shot crawl from the Slack API, MCP, desktop cache, an external provider, or API plus desktop sources. |
| `import` | Import a Slack export ZIP or extracted directory. |
| `tail` | Listen for live events through Slack Socket Mode and run periodic repair syncs. |
| `watch` | Refresh Slack Desktop state on an interval. |
| `files` | List stored Slack file metadata or fetch media into the local cache. |

`sync` is incremental by default. `--full` deliberately removes the local history cursor; `--latest-only` skips channels that do not already have local history.

```sh
slacrawl sync --source bot
slacrawl sync --source bot --latest-only --with-media
slacrawl sync --source mcp --workspace T01234567
slacrawl sync --source wiretap
```

Slack API pagination stops with a `repeated cursor` error if a response revisits a page cursor, preventing a stuck sync. Pages already stored remain available for the next incremental sync.

Import a workspace export with an explicit workspace ID:

```sh
slacrawl import ./my-export.zip --workspace T01234567
slacrawl import ./extracted-export --workspace T01234567 --dry-run
```

Desktop and Socket Mode loops serve different sources:

```sh
slacrawl tail --repair-every 30m
slacrawl watch --desktop-every 5m
```

`tail` requires an app token. `watch` reads local Slack Desktop state and refreshes every workspace in the signed-in profile unless `--workspace` restricts it.

## Browse and query

| Command | Purpose |
| --- | --- |
| `search` | Search message text with FTS5 and substring fallback. |
| `tui` | Browse archived messages in the terminal. |
| `messages` | List messages with workspace and channel filters. |
| `mentions` | List extracted mention records. |
| `users` | List synced users; the default limit is 100. |
| `channels` | List synced channels; the default limit is 100. |
| `sql` | Run read-only SQL against the archive. |

```sh
slacrawl search --workspace T01234567 "incident"
slacrawl messages --channel C12345678 --limit 20
slacrawl mentions --limit 20
slacrawl channels --limit 200
slacrawl users --limit 200
slacrawl sql 'select channel_id, count(*) as messages from messages group by channel_id order by messages desc limit 10;'
```

## Reports and analytics

| Command | Purpose |
| --- | --- |
| `report` | Summarize archive activity, storage, and git-share freshness. |
| `digest` | Summarize per-channel activity for a time window. |
| `analytics digest` | Run the digest through the grouped analytics interface. |
| `analytics quiet` | Find quiet channels in a time window. |
| `analytics trends` | Group message trends by week. |

```sh
slacrawl report
slacrawl digest --since 7d
slacrawl analytics quiet --since 30d
slacrawl analytics trends --weeks 8
```

Analytics commands accept workspace filters; digest and trends also accept channel filters.

## Retention

`purge` previews or deletes messages and message-owned records before an exclusive cutoff. It accepts either `--older-than` or `--before`; deletion requires `--force`.

```sh
slacrawl purge --older-than 90d
slacrawl purge --workspace T01234567 --before 2026-01-01
slacrawl purge --older-than 90d --force --vacuum
```

See [Retention purge](retention.md) for thread behavior, event compaction, cached media, and restore semantics.

## Git snapshots

| Command | Purpose |
| --- | --- |
| `publish` | Export the local archive into a git repository and optionally commit, tag, and push it. |
| `subscribe` | Configure a reader, clone a snapshot repository, and import it. |
| `update` | Pull and merge a newer snapshot, or explicitly restore an exact snapshot. |

See [Git archive sharing](git-archive-sharing.md) for setup and command examples.

## Output modes

The global output flags are:

- `--format text` for the terminal-oriented default
- `--format json` or `--json` for machine-readable output
- `--format log` for line-oriented automation output
- `--no-color` to force plain text

Color also turns off when stdout is not a TTY or `NO_COLOR=1` is set. `metadata --json`, `status --json`, and `doctor --json` expose control and status payloads for automation.

Interactive release checks are cached daily. Set `SLACRAWL_NO_UPDATE_CHECK=1` or `CRAWLKIT_NO_UPDATE_CHECK=1` to suppress them.
Release-check HTTP requests time out after 30 seconds.

## Shell completion

Generate completion scripts for Bash or Zsh:

```sh
slacrawl completion bash > /usr/local/etc/bash_completion.d/slacrawl
mkdir -p "${HOME}/.zsh/completions"
slacrawl completion zsh > "${HOME}/.zsh/completions/_slacrawl"
```

From a source checkout, `make completion` writes both files under `dist/completions/`.

## Common workflows

An API-backed archive usually follows this sequence:

```sh
slacrawl init
slacrawl doctor
slacrawl sync --source bot
slacrawl status
slacrawl report
slacrawl search "incident"
```

For a seeded archive that only needs fresh deltas:

```sh
slacrawl sync --source bot --latest-only
slacrawl digest --since 7d
```
