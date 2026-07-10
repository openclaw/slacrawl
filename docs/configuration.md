# Configuration

`slacrawl` is configured with TOML at `~/.slacrawl/config.toml` by default.

Config path resolution, runtime directories, status payloads, and token
diagnostic formatting are normalized through `crawlkit`. Slack token scopes,
workspace selection, API/Desktop source behavior, and schema compatibility stay
in `slacrawl`.

The config is designed to work with safe defaults:

- SQLite lives under `~/.slacrawl/`
- Slack Desktop is enabled by default
- the desktop path is auto-detected when left blank
- Slack tokens are resolved from environment variables
- external providers receive only explicitly allowlisted environment additions

## Example

```toml
version = 1
workspace_id = ""
db_path = "~/.slacrawl/slacrawl.db"
cache_dir = "~/.slacrawl/cache"
log_dir = "~/.slacrawl/logs"

[[workspaces]]
id = "T01234567"
default = true
# uses:
# SLACK_T01234567_BOT_TOKEN
# SLACK_T01234567_APP_TOKEN
# SLACK_T01234567_USER_TOKEN

[slack.bot]
enabled = true
token_env = "SLACK_BOT_TOKEN"

[slack.app]
enabled = true
token_env = "SLACK_APP_TOKEN"

[slack.user]
enabled = true
token_env = "SLACK_USER_TOKEN"

[slack.desktop]
enabled = true
path = ""

[slack.mcp]
enabled = true
base_url = "https://chatgpt.com/backend-api/wham/apps"
auth_path = "~/.codex/auth.json"
token_env = "CODEX_APPS_ACCESS_TOKEN"
account_id_env = "CODEX_APPS_ACCOUNT_ID"
protocol_version = "2025-03-26"
connector_id = ""
channel_types = "public_channel,private_channel"
page_size = 100
search_limit = 20
max_pages = 250

[sync]
concurrency = 4
repair_every = "30m"
desktop_refresh_every = "5m"
full_history = true

[search]
default_mode = "auto"

[share]
remote = ""
repo_path = "~/.slacrawl/share"
branch = "main"
auto_update = true
stale_after = "15m"
```

## Workspace Selection

`workspace_id` remains the default CLI workspace.

Use `[[workspaces]]` when you want separate bot/app/user tokens per Slack workspace, especially for multi-workspace API sync and live tailing:

```toml
[[workspaces]]
id = "T01234567"
default = true

[[workspaces]]
id = "T08976543"
bot_token_env = "SLACK_CLIENT_BOT_TOKEN"
app_token_env = "SLACK_CLIENT_APP_TOKEN"
user_token_env = "SLACK_CLIENT_USER_TOKEN"
```

Behavior:

- each workspace automatically tries `SLACK_<WORKSPACE_ID>_BOT_TOKEN`, `SLACK_<WORKSPACE_ID>_APP_TOKEN`, and `SLACK_<WORKSPACE_ID>_USER_TOKEN`
- top-level `enabled` flags are inherited, so you do not need to repeat `enabled = true` for every workspace
- `bot_token_env`, `app_token_env`, and `user_token_env` are optional overrides when you do not want the default env naming convention
- `sync --source bot` without `--workspace` runs against every configured `[[workspaces]]` entry
- `tail` without `--workspace` starts one live tail per configured `[[workspaces]]` entry
- `search`, `messages`, `mentions`, `users`, and `channels` accept `--workspace` to filter the shared SQLite database
- if `[[workspaces]]` is empty, the legacy top-level `[slack.*]` token config is used

## Visibility Boundaries

One config/database should represent one Slack visibility boundary: the messages visible to one bot/account/profile. Use ingestion sources to decide how that archive is populated:

- `sync --source bot` is an alias for `sync --source api` and uses Slack bot/user tokens
- `sync --source mcp` fetches from a Slack connector exposed by the configured HTTP JSON-RPC MCP gateway
- `sync --source provider:<name>` imports a configured external archive through a trusted local subprocess
- `sync --source wiretap` is an alias for `sync --source desktop` and reads the local Slack Desktop cache
- `sync --source all` runs token-backed sync first, then desktop enrichment; external providers remain explicit
- `[share]` backs up the current DB and safely merges snapshots by default; it is not a second Slack data source
- exact latest or historical replacement requires `update --restore`

Keep company and personal Slack archives in separate configs, DBs, and git remotes:

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

## MCP Connector Source

MCP sync is an additional ingestion source; all reads still use the local slacrawl database. It discovers Slack tools with `tools/list`, calls the connector for users, channels, channel history, and threads, then normalizes the results into the same archive schema used by API and desktop sync.

```bash
slacrawl sync --source mcp --workspace T01234567
slacrawl sync --source mcp --workspace T01234567 --channels C01234567 --since 1772574099.659199
```

The workspace ID is required because connector responses do not reliably carry archive ownership. `connector_id` is optional; when empty, tool discovery matches Slack tools by their names and metadata. Set it when multiple Slack connectors are exposed by one gateway.

The default `transport = "http"` uses Codex's connector gateway:

```toml
[slack.mcp]
enabled = true
transport = "http"
base_url = "https://chatgpt.com/backend-api/wham/apps"
auth_path = "~/.codex/auth.json"
```

HTTP authentication order:

1. The configured `token_env` and optional `account_id_env`.
2. `CODEX_APPS_ACCESS_TOKEN` or `CODEX_CONNECTORS_TOKEN`.
3. The configured `auth_path`, using Codex's `tokens.access_token` and optional `tokens.account_id` fields.

`max_pages` bounds each users, channels, channel-history, and thread pagination loop; hitting the bound returns an error instead of silently accepting an incomplete page set. The Codex HTTP connector accepts at most 20 channel or user search results per request. Explicit channel IDs avoid global channel and user enumeration. Normal MCP sync overlaps the latest stored message timestamp per channel by one hour and rechecks persisted thread roots because Slack does not move an old root into channel history when it receives a new reply; `--full` removes the local channel cursor, while `--latest-only` skips channels with no local history. MCP is an explicit source and is not included in `--source all`.

The connector's channel search response may omit privacy metadata. Those channels are stored with kind `mcp_channel` rather than being assumed public; they remain locally searchable but are not treated as public channels by archive export logic.

For the archived MCP reference Slack server, use stdio and export the server's required `SLACK_BOT_TOKEN` and `SLACK_TEAM_ID` variables before running slacrawl:

```toml
[slack.mcp]
enabled = true
transport = "stdio"
command = "npx"
args = ["-y", "@modelcontextprotocol/server-slack"]
env_allowlist = ["SLACK_BOT_TOKEN"]
page_size = 100
search_limit = 100
max_pages = 250
```

The subprocess receives a minimal environment plus known Slack/Codex token variables and any names listed in `env_allowlist`; secrets are not passed through TOML fields. The reference server exposes public or explicitly configured channels and does not paginate channel history. Therefore each sync imports only the latest `page_size` messages returned by `slack_get_channel_history`; `--since` filters that fetched window locally and `--full` cannot extend the server's history window. Channel and user listing still paginate normally. The npm package is deprecated and its upstream repository is archived, but this adapter supports its published tool contract.

Tool discovery selects either the Codex Slack connector contract or the reference `slack_list_channels`, `slack_get_channel_history`, `slack_get_thread_replies`, and `slack_get_users` contract.

## External Archive Providers

Use `[[providers]]` to adapt another local archive to the canonical SQLite
schema without adding a source-specific integration to `slacrawl` itself.
Each provider is an explicit sync source and is not included in `--source all`.

```toml
[[providers]]
name = "archive"
command = "/usr/local/bin/archive-provider"
args = ["provide", "--format", "jsonl"]
env_allowlist = ["ARCHIVE_DB_PATH"]
source_rank = 5
batch_size = 1000
```

```bash
slacrawl sync --source provider:archive --workspace T01234567
slacrawl sync --source provider:archive --workspace T01234567 --channels C01234567
slacrawl sync --source provider:archive --workspace T01234567 --full
slacrawl sync --source provider:archive --workspace T01234567 --limit 100
```

Configuration rules:

- `name` is normalized to lowercase and must be unique; whitespace, slashes, and colons are rejected
- `command` is required; `~` or a leading `~/` is expanded, then the path must be absolute
- `args` are passed directly to the executable without shell parsing
- `env_allowlist` names additional variables to copy from the parent environment; values do not belong in TOML
- the subprocess otherwise receives only ordinary runtime variables such as `HOME`, `PATH`, temporary-directory, user, shell, and platform variables when present
- `source_rank` must be greater than `2`; lower numeric ranks win, so provider messages cannot replace canonical rank `1` or `2` data; equal ranks may replace
- `batch_size` defaults to `1000`, accepts `1` through `100000`, and is forwarded to the provider as a preferred upstream batch size

### JSONL protocol

`slacrawl` writes exactly one JSON object followed by a newline to provider stdin,
then closes stdin. Example:

```json
{"type":"request","protocol":"slacrawl-provider-v1","workspace_id":"T01234567","channels":["C01234567"],"checkpoint":"opaque-provider-state","batch_size":1000,"limit":100}
```

`limit` is omitted when no positive limit was requested. The other optional
fields may also be omitted when empty. `checkpoint` is an opaque string that
the provider previously emitted; `slacrawl` never parses it.

Provider stdout is JSONL. Records must appear in this order:

1. One `hello` record before any other output.
2. Zero or more data and checkpoint records.
3. Exactly one terminal `done` record, followed by EOF.

An example response looks like:

```jsonl
{"type":"hello","protocol":"slacrawl-provider-v1","provider":"archive-cache"}
{"type":"workspace","id":"T01234567","name":"Example Workspace","raw_json":{}}
{"type":"user","workspace_id":"T01234567","id":"U01234567","name":"example","raw_json":{}}
{"type":"channel","workspace_id":"T01234567","id":"C01234567","name":"general","kind":"public_channel","raw_json":{}}
{"type":"message","workspace_id":"T01234567","channel_id":"C01234567","ts":"1772574099.659199","user_id":"U01234567","thread_ts":"","text":"Example message","raw_json":{}}
{"type":"checkpoint","entity_type":"workspace","entity_id":"T01234567","value":"opaque-next-state"}
{"type":"done","records":4}
```

`done.records` counts `workspace`, `channel`, `user`, and `message` records; it
does not count `hello`, `checkpoint`, or `done`. Optional fields by record type:

- `workspace`: `name`, `domain`, `enterprise_id`, `raw_json`
- `channel`: `name`, `kind`, `topic`, `purpose`, `is_private`, `is_archived`, `is_shared`, `is_general`, `raw_json`
- `user`: `name`, `real_name`, `display_name`, `title`, `is_bot`, `is_deleted`, `raw_json`
- `message`: `user_id`, `thread_ts`, `text`, `raw_json`
- `done`: `skipped_users`, `skipped_channels`, `skipped_bad_message_id`, `skipped_duplicate_identity`, `skipped_missing_channel`, `skipped_missing_timestamp_sort_key`, `rounded_thread_messages`, `rounded_threads`, `unresolved_thread_messages`, and `unresolved_threads`

Unknown JSON fields are ignored.

Required identity fields are the requested workspace `id`; channel and user
`workspace_id` plus `id`; and message `workspace_id`, `channel_id`, and Slack
`ts`. A message channel must already exist in the database or have appeared
earlier in the stream. An unknown nonempty message user ID is preserved as a
sparse workspace-bound profile that a later real user record may enrich. Empty
names fall back to their IDs, empty channel kinds become `provider_channel`,
and missing or `null` `raw_json` becomes `{}`.

`slacrawl` builds repaired, normalized search text from each message, extracts
mentions, updates FTS, and uses `source_rank` reconciliation for canonical
message rows. Metadata emitted by a provider is sparse enrichment and does not
replace richer existing workspace, channel, or user metadata.

### Checkpoints, scopes, and limits

The normal unfiltered incremental run stores one checkpoint under
`provider:<name>` and the workspace ID. Any `--since`, `--full`,
`--latest-only`, channel filter, exclusion, or `--limit` gets a deterministic
scope-specific checkpoint. This prevents a bounded validation import or a
partial backfill from advancing the normal incremental cursor.

A `checkpoint` record flushes pending records and the checkpoint in one SQLite
transaction. Emit it only after all records covered by its value. If the
provider later exits nonzero or omits `done`, the command fails, but already
committed records and checkpoints remain available for the next retry. A
successful unbounded `--full` promotes its last checkpoint to the matching
incremental scope; a limited full run does not.

`--limit` is intended for validation imports. `slacrawl` forwards a positive
message limit and fails if the provider emits more messages. The limit applies
only to `message` records, not catalog records. Providers must also interpret
and honor `since`, `full`, `latest_only`, `channels`, and `exclude_channels`;
`slacrawl` forwards those values but cannot infer source-specific filtering.

Incremental provider imports enforce stored channel retention floors. `--full`,
or an explicit `--since` older than a channel's floor, is treated as a deliberate
restore and may reintroduce purged history.

### Safety and failure behavior

- treat the executable as trusted code inside the archive visibility boundary
- `slacrawl` restricts environment forwarding but does not sandbox the executable; it retains the caller's filesystem and network permissions
- allowlist only variables the provider needs, especially credentials or archive paths
- keep separate configs and databases for archives with different visibility
- the first record must be `hello` with protocol `slacrawl-provider-v1`
- cross-workspace records, missing required identities or channels, protocol mismatches, output after `done`, and record-count mismatches fail closed
- the process must emit `done`, close stdout, and exit zero for the run to succeed
- EOF without `done` flushes pending records before reporting failure; exceeding `--limit` likewise flushes pending records through the limit before failing
- provider stderr is capped and attached to failures for diagnostics
- provider v1 transfers metadata and message text, not file blobs

## Git Archive Sharing

Use `[share]` when you want one machine to publish a private Slack archive snapshot and other machines to query it locally without Slack API credentials.

```toml
[share]
remote = "git@github.com:your-org/private-slacrawl-archive.git"
repo_path = "~/.slacrawl/share"
branch = "main"
auto_update = true
stale_after = "15m"
```

Behavior:

- `publish` exports gzipped JSONL table shards plus `manifest.json` into `repo_path`
- current snapshots contain metadata tables only; future file/media blobs must be gzip-compressed in the share repo, with raw-media import kept for backward compatibility
- `subscribe` writes a git-reader config, disables Slack API and desktop sources for that config, clones the repo, and imports the snapshot
- pass `--db` to `subscribe` when you want the reader archive to use a non-default SQLite file
- `update` pulls and imports only when the manifest changed
- `status`, `search`, `messages`, `mentions`, `sql`, `users`, `channels`, and `report` auto-refresh stale git-backed snapshots before querying when `auto_update = true`
- `stale_after` controls how old the last successful import can be before the next read pulls/imports again
- `status` and `doctor` show the configured share repo plus last import / manifest freshness details

## Token Sources

Each Slack token source is controlled independently.

Text normalization notes:

- malformed UTF-8 is repaired before indexing
- compatibility forms are normalized with NFKC
- zero-width and non-printable control noise is stripped from indexed text
- weird spacing is collapsed so FTS and mentions stay queryable even when Slack/Desktop payloads are messy

### Bot token

Use the bot token for normal API sync:

- channel discovery
- users snapshot
- channel history

Disable it entirely if you want desktop-only operation:

```toml
[slack.bot]
enabled = false
token_env = "SLACK_BOT_TOKEN"
```

### App token

Use the app token only when you want live Socket Mode tailing:

```toml
[slack.app]
enabled = true
token_env = "SLACK_APP_TOKEN"
```

If app tailing is not needed, disable it:

```toml
[slack.app]
enabled = false
token_env = "SLACK_APP_TOKEN"
```

### User token

The user token is optional, but it upgrades historical thread coverage for public and private channels.

```toml
[slack.user]
enabled = true
token_env = "SLACK_USER_TOKEN"
```

If you do not want user-token access at all:

```toml
[slack.user]
enabled = false
token_env = "SLACK_USER_TOKEN"
```

## Desktop Source

Desktop ingestion is optional and read-only.

```toml
[slack.desktop]
enabled = true
path = ""
```

Behavior:

- `enabled = true` turns on desktop sync support
- `path = ""` auto-detects the supported macOS or Linux Slack Desktop path
- `path = "/custom/path"` overrides detection

To disable desktop ingestion completely:

```toml
[slack.desktop]
enabled = false
path = ""
```

## Sync Settings

### `repair_every`

Used by `tail` to run periodic API reconciliation during live sync.

```toml
[sync]
repair_every = "30m"
```

### `desktop_refresh_every`

Used by `watch` to periodically refresh local Slack Desktop state into SQLite.

```toml
[sync]
desktop_refresh_every = "5m"
```

### `concurrency`

Used by API sync to fan out channel history fetches across workers. Keep the default unless you have a reason to tune it for a specific workspace.

Notes:

- higher values increase API fan-out, not write parallelism inside SQLite
- useful mainly for multi-channel API sync, not single-channel runs
- `--concurrency` on the CLI overrides the config value for that run

### `latest-only`

Use `sync --latest-only` when you want to refresh only channels that already have a stored cursor.

Notes:

- useful for fast publisher jobs that already seeded history once
- channels with no local history are skipped instead of triggering a first-time backfill
- `--full` overrides this behavior and still does the full crawl

## Recommended Profiles

### Desktop only

```toml
[slack.bot]
enabled = false
token_env = "SLACK_BOT_TOKEN"

[slack.app]
enabled = false
token_env = "SLACK_APP_TOKEN"

[slack.user]
enabled = false
token_env = "SLACK_USER_TOKEN"

[slack.desktop]
enabled = true
path = ""
```

### API sync without live tail

```toml
[slack.bot]
enabled = true
token_env = "SLACK_BOT_TOKEN"

[slack.app]
enabled = false
token_env = "SLACK_APP_TOKEN"

[slack.user]
enabled = true
token_env = "SLACK_USER_TOKEN"
```

### API sync with live tail and desktop refresh

```toml
[slack.bot]
enabled = true
token_env = "SLACK_BOT_TOKEN"

[slack.app]
enabled = true
token_env = "SLACK_APP_TOKEN"

[slack.user]
enabled = true
token_env = "SLACK_USER_TOKEN"

[slack.desktop]
enabled = true
path = ""

[sync]
repair_every = "30m"
desktop_refresh_every = "5m"
```
