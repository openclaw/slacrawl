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

## Offline projections

These commands use explicit paths and work without HOME or a Slacrawl config:

```sh
slacrawl --json export prepare --db archive.db --selection selection.json --out private-plan.json
slacrawl --json export build --db archive.db --plan private-plan.json --out new-projection
slacrawl --json export verify --db archive.db --plan private-plan.json --dir new-projection
```

Put global output flags before `export`, or use `--format`/`--json` after the
subcommand. `export --json prepare` is unsupported. Global `--config` is ignored;
`--config` after an export subcommand is unsupported. The export path does not
resolve default config, initialize or repair the archive, auto-import, sync,
check releases, contact Slack, or invoke Git.

Create a private selection document in this field order. Formatting whitespace
is allowed; field names, string escapes and required fields use the exact typed
JSON spelling. Unknown, duplicate, case-varied and missing fields are rejected.

```json
{
  "workspace_id": "TTEST",
  "workspace_label": "reviewed workspace",
  "channels": [{"channel_id": "CPUBLIC", "label": "reviewed channel"}],
  "messages": [{
    "channel_id": "CPUBLIC",
    "ts": "1767312000.000002",
    "text": {"mode": "keep", "replacement": null}
  }]
}
```

Use `{"mode":"replace","replacement":"reviewed text"}` to replace text;
`""` is an explicit empty replacement. Keep requires `replacement:null`.
Both arrays must select at least one entry. Empty labels default to IDs. Select
any reply's eligible parent explicitly. Selected channels need retained native
public-channel evidence; sparse or conflicting metadata cannot qualify.

Prepare writes a private version 1 envelope containing `version`,
`producer_revision` and `selection` (the bound core plan). Do not hand-edit it:
it requires exact compact JSON plus one LF, including all generated hashes and
fields. It is not a shareable artifact. Selection and plan files must be regular nonsymlink
files. The plan and artifact destinations must be new, with existing parents;
failed writes remain for inspection, and a retry needs a new destination.

Prepare and build require a clean Git checkout stamped into the binary with
`go build -buildvcs=true ./cmd/slacrawl`. A dirty checkout or binary without
that metadata fails; some module-version installs omit it. Build must use the
same revision as prepare. Verify can use a newer binary without matching its
revision to the plan, and does not require the old producer binary. Both commands
recheck the plan against the current archive, so changed selected rows require
new preparation and review.

Prepare prints channel/message counts. Build and verify print
`manifest_sha256`, `messages_sha256`, `manifest_bytes`, `messages_bytes` and `rows`.
The two-file artifact contains only explicit labels, identities and selected
message fields. Receipts describe the bytes observed; they do not certify
lifetime DM origin, quoted private content or permission to publish. Operators
must review selection, labels and kept/replacement text. See
[projection limits](git-archive-sharing.md#offline-selected-projections).

## Ingest and refresh

| Command | Purpose |
| --- | --- |
| `sync` | Run a one-shot crawl from the Slack API, MCP, desktop cache, an external provider, or API plus desktop sources. |
| `import` | Import a Slack export ZIP or extracted directory. |
| `tail` | Listen for live events through Slack Socket Mode and run periodic repair syncs. |
| `watch` | Refresh Slack Desktop state on an interval. |
| `files` | List stored Slack file metadata or fetch media into the local cache. |

`sync` is incremental by default. `--full` deliberately removes the local history cursor; `--latest-only` skips channels that do not already have local history.

`sync --source api` and its `bot` alias use the configured bot, otherwise the
user token, as primary. An invalid bot never triggers user fallback. User-only
sync can read permitted channels without joining them; Tail and periodic Tail
repair still require the bot, with an app token for Socket Mode.

Doctor reports global and named-workspace thread capabilities separately and
combines recent bot/user API channel skips. It does not rewrite stored sync
status. See [Doctor coverage ownership](configuration.md#doctor-coverage-ownership).

`sync --since` accepts a finite Slack timestamp or RFC3339 time. Invalid numeric
values such as `NaN` and infinity fail before opening the archive.

```sh
slacrawl sync --source bot
slacrawl sync --source bot --latest-only --with-media
slacrawl sync --source mcp --workspace T01234567
slacrawl sync --source wiretap
```

Slack API pagination, including the member directory (`users.list`), stops with a `repeated cursor` error if a response revisits a page cursor, preventing a stuck sync. Pages already stored remain available for the next incremental sync.

Native reference MCP history/replies require `ok=true`. If a response reports
more pages or a Slack history/message limit, `sync --source mcp` (alias
`connector`) keeps valid fetched writes but exits with an actionable error and
preserves the previous successful workspace sync record. See
[native response coverage](configuration.md#native-response-coverage).

MCP incremental scans use local history checkpoints. Newer replies and rows from
other sources do not advance their cutoff. The first scan without a checkpoint
starts unbounded subject to retention; failed history retries its pending interval,
and completed empty history is recorded. See [MCP history checkpoints](configuration.md#mcp-history-checkpoints).

Ordinary API sync revisits retained thread roots and resumes saved replies work.
`--full` does too; explicit `--since`, `--full --since` and Tail repair leave the
ordinary backlog untouched. Replies need user authentication; unavailable replies
keep partial coverage and pending work. See [Retained API threads](configuration.md#retained-api-threads).

Import a workspace export with an explicit workspace ID:

```sh
slacrawl import ./my-export.zip --workspace T01234567
slacrawl import ./extracted-export --workspace T01234567 --dry-run
```

Set `[sync].include_dms = false` in that config to exclude known DMs before
importing metadata or messages. Public/private channels require consistent
workspace JSON catalog or native type evidence; unknown types and ambiguous
ownership stop the import. `--force` does not bypass this policy.

Directory reads remain confined to the export root. Contained aliases for the
same conversation and a linked outer root are supported; two conversations
cannot share a locator, directory, or message file. ZIP imports reject ambiguous
entries and shared conversation payload ranges.

`--dry-run` uses an existing archive read-only, or treats a missing archive as
empty. It does not initialize runtime directories, migrate the database, or
repair its search index. A pending repair or incompatible old schema can fail
read-only validation. Ordinary imports still open the archive writable after
catalog/locator admission; a later body error can leave that initialization and
previously committed 500-message batches.

The selected files and catalog are fixed during preparation. Changed, replaced,
or missing selected files fail; new files require a new import. See
[Slack export admission](configuration.md#slack-export-admission) for format and
privacy limits.

To import a [Slackdump](https://github.com/rusq/slackdump) archive, first convert
it to a standard Slack export. Use a copy of the source archive: Slackdump may
migrate database inputs while opening them.

```sh
slackdump convert -f export -files=false -avatars=false -o ./export.zip ./archive-copy
slacrawl import ./export.zip --workspace T01234567
```

Use `-o ./export` for directory output. This path is verified with Slackdump
[`f7319928`](https://github.com/rusq/slackdump/commit/f7319928b0993b23d7e9bd8af5e4c69b6f1d2af4)
database and chunk-directory archives, including threads across export dates.
That converter groups files by the America/Los_Angeles calendar and preserves
message timestamps. The stored fixtures are synthetic; this does not certify
live capture completeness, attachments or avatars.

Explicit `include_dms = false` applies the import policy above and normalizes
native private-channel evidence. With omitted/true compatibility settings, a
modern private channel in `channels.json` retains kind `public_channel` and
`is_private = true`. DM exclusion does not remove older archive rows or certify
that an archive is safe to publish.

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

`users` and `channels` accept filters before or after their optional query, for
example `slacrawl users alice --workspace T01234567`. Missing filter values
return an error. Search terms and filter values can contain the word `help`;
use `slacrawl search -- help` to search for that word by itself.

`tui --limit N` keeps up to N visible page messages plus their available thread
parents, so a reply retains its context even on a short page. Draft and system
message filters also apply to parents.

`sql` accepts one read-only `SELECT` query, including `WITH` queries, leading
comments, and SQLite quoted identifiers. Additional statements are rejected.
Give every result column a unique name with `AS` when needed; duplicate names
return an error because output rows use column names as keys.
With no query argument, `sql` reads stdin; Ctrl-C cancels that read and exits 130.

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

`analytics digest --help`, `analytics quiet --help`, and `analytics trends --help`
work before configuration has been created and also when the config is malformed.

Digest and trend windows include their exact microsecond boundaries. Quiet-channel
reports ignore messages after their advertised end time. Thread totals count each
channel's root independently, using either its thread marker or reply metadata.

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

`publish` rejects explicit `[sync].include_dms = false` before opening the
archive, locking the media cache, Git operations or snapshot writes. Keep the
archive local; `--no-commit`, `--no-media` and tag/push options do not bypass this
gate. Omitted/true retains unfiltered private snapshots, including stored DMs
and drafts. This does not certify an archive as safe to publish.

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
