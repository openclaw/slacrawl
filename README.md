# slacrawl 💾 — Your Slack history, on your terms.

[![CI](https://img.shields.io/github/actions/workflow/status/openclaw/slacrawl/ci.yml?branch=main&style=flat-square&label=ci)](https://github.com/openclaw/slacrawl/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/openclaw/slacrawl?style=flat-square)](https://github.com/openclaw/slacrawl/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/openclaw/slacrawl?style=flat-square)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey?style=flat-square)](https://github.com/openclaw/slacrawl/releases/latest)
[![License](https://img.shields.io/github/license/openclaw/slacrawl?style=flat-square)](LICENSE)
[![Homebrew](https://img.shields.io/badge/homebrew-openclaw%2Ftap-FBB040?style=flat-square&logo=homebrew&logoColor=black)](https://github.com/openclaw/homebrew-tap)

`slacrawl` mirrors Slack workspace data into local SQLite for full-text search, terminal browsing, reporting, and read-only SQL. It is for people who want a queryable Slack archive that stays under their control.

<p align="center">
  <img src="screenshot.png" alt="slacrawl terminal interface showing archived Slack messages" width="801">
</p>

## Install

Homebrew is the shortest path on macOS or Linux:

```sh
brew install openclaw/tap/slacrawl
```

The tap can trail the newest release. [GitHub Releases](https://github.com/openclaw/slacrawl/releases/latest) provides signed and notarized macOS archives, Linux archives, and Debian/RPM packages for AMD64 and ARM64.

To build the latest source, install Go 1.27.0 or newer, then run:

```sh
git clone https://github.com/openclaw/slacrawl.git
cd slacrawl
make build
./bin/slacrawl --help
```

The repository also includes a [`Dockerfile`](Dockerfile) for container builds.

## Quick start

Set `SLACK_BOT_TOKEN` to a bot token that can read the workspace, then replace the example workspace and channel IDs:

```sh
slacrawl init --workspace T01234567
slacrawl doctor
slacrawl sync --source bot --channels C01234567
slacrawl search "incident"
slacrawl tui
```

`init` writes `~/.slacrawl/config.toml`; database-backed commands use `~/.slacrawl/slacrawl.db` by default. A user token is optional for broader thread and DM coverage; an app token is only needed for live Socket Mode tailing.

Already have a Slack export? Import its ZIP or extracted directory instead of syncing from the API:

```sh
slacrawl import ~/Downloads/slack-export.zip --workspace T01234567
```

## Choose an ingestion source

Every source feeds the same SQLite archive and search index.

| Source | Command | Use it for |
| --- | --- | --- |
| Slack API | `slacrawl sync --source bot` | Token-backed channel history, users, threads, and incremental refreshes |
| Slack Desktop | `slacrawl sync --source wiretap` | Read-only recovery from local macOS or Linux desktop caches |
| MCP connector | `slacrawl sync --source mcp --workspace T01234567` | Connector-backed history without a direct Slack API integration |
| External provider | `slacrawl sync --source provider:archive --workspace T01234567` | A trusted local JSONL adapter for another archive |
| Slack export | `slacrawl import <path> --workspace T01234567` | ZIP or directory exports |

Treat one config and database as one visibility boundary. Keep personal and company archives in separate configs, database paths, and share remotes. See [Configuration](docs/configuration.md) for tokens, multiple workspaces, MCP, external providers, media caching, and source precedence; see [Desktop mode](docs/desktop-mode.md) for local-cache coverage and limitations.

## Explore the archive

Search uses SQLite FTS5 with a substring fallback. The same archive is available through the TUI, structured commands, and read-only SQL:

```sh
slacrawl report
slacrawl messages --limit 20
slacrawl sql 'select channel_id, count(*) from messages group by channel_id;'
slacrawl --json status
```

Use `--format text`, `--format json`, or `--format log` on commands that support structured output. Color turns off when stdout is not a TTY; `--no-color` and `NO_COLOR=1` force plain output.

## Keep it current

Run an incremental API refresh after the first sync:

```sh
slacrawl sync --source bot
```

Use `--latest-only` to update only channels that already have local history, `tail` for Socket Mode events, or `watch` for recurring desktop-cache refreshes. Ordinary incremental sync preserves retention cutoffs; `--full`, an older explicit `--since`, desktop ingestion, or imports can deliberately restore older records.

## Share an archive

One machine can publish compressed, git-backed snapshots while other machines subscribe and query locally without Slack credentials. Routine updates merge safely and preserve destination-only rows; exact replacement requires `update --restore`.

See [Git archive sharing](docs/git-archive-sharing.md) for publisher and subscriber setup, snapshot tags, media behavior, and restore semantics.

## Retention

`purge` previews its effect unless `--force` is supplied:

```sh
slacrawl purge --older-than 90d
slacrawl purge --older-than 90d --force --vacuum
```

Threads are retained or removed as a unit based on the parent timestamp. See [Retention purge](docs/retention.md) for workspace scoping, message-event compaction, cached media, and git-backed archive behavior.

## Commands

| Area | Commands |
| --- | --- |
| Setup and health | `init`, `doctor`, `status`, `check-update` |
| Ingest and refresh | `sync`, `import`, `tail`, `watch`, `files` |
| Browse and query | `search`, `tui`, `messages`, `mentions`, `users`, `channels`, `sql` |
| Reports | `report`, `digest`, `analytics` |
| Git snapshots | `publish`, `subscribe`, `update` |
| Automation | `metadata`, `completion` |

The [command reference](docs/commands.md) describes every command, output modes, shell completion, and common workflows. `slacrawl <command> --help` is the authoritative flag reference.

## Development

```sh
make build
make test
make check
```

`make check` runs the local CI gates, including formatting, vet, vulnerability and dead-code checks, tests, CLI smoke tests, and a release snapshot. Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before changing behavior and [`SPEC.md`](SPEC.md) for the implementation contract.

## License

MIT. Built by [Vincent Koc](https://github.com/vincentkoc).
