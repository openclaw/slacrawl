# Configuration

`slacrawl` is configured with TOML at `~/.slacrawl/config.toml` by default.

Starting with Slacrawl v0.9.0, config-saving commands use owner-only permissions
(`0600`) on POSIX systems, including when saving existing configs. Saving removes
group/other read access; shared-account configurations must plan for owner access.

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

## Archive Schema Upgrades

Starting with Slacrawl v0.9.0, the first writable open of a pre-v8 database upgrades
its SQLite schema version to 8 in a transaction. This is separate from the TOML
configuration version.
Read-only inspection with automatic share updates disabled can inspect a valid
v7 archive without migrating it; `init` only writes configuration. A command such
as `sync` opens the archive writable and can migrate it even if it subsequently
fails for missing credentials.

The one-time upgrade removes `history_coverage_v1` checkpoints for the exact
`api-bot` and `api-user` sources. Pre-v8 checkpoints cannot reliably distinguish
locally completed scans from checkpoints imported from another archive. Messages,
retention floors and seeds, provider/MCP state, and other sync state are retained.
The migration performs no network requests or backfill and does not mark any
history complete. Later writable opens preserve newly earned checkpoints.

The next ordinary API sync treats that coverage as unknown and may repeat history
requests from the existing retention floor. Without a floor, it can scan all
accessible history, with corresponding API cost and rate-limit delays. A newer
saved message does not prove that older history was read. Successful scans earn
local completion checkpoints; interrupted scans retain their pending interval.
`--latest-only` selects previously observed channels; it does not bound the
history interval or request count.

API history acquires its current lower bound, upper request horizon and a fresh
attempt generation in one archive transaction. The exact primary source,
workspace, channel and normalized `--since` identify the scope. A newer attempt
supersedes the older one: stale history/replies responses, thread work, skip/join
records and completion cannot overwrite its state. Earlier committed pages stay
available, and the superseded caller reports failure. An already dispatched
external join cannot be undone.

A delayed clock cannot shrink the upper horizon inherited from completed or
active work. Completion records the horizon actually requested, including an
empty scan. Ordinary retries apply the current retention floor and never inherit
Full's restore permission; explicit Since still wins over Full, and Full wins
over LatestOnly. Valid old v8 checkpoints acquire generation and upper fields
on their next scan; the SQLite version and share format do not change.

History acquisition validates only its selected canonical checkpoint and owned
channel. Coverage decisions separately scan retained records for the exact API
sources and history type. Every key must be canonical; workspace-only decisions
ignore a foreign value only after validating its key. Sync and CLI Doctor use an
archive-wide view, while repair and Full skip cleanup use the selected workspace.
Malformed relevant records fail without resetting or deleting them. Pending work,
including an empty pending bound, or a record without completed history keeps
coverage partial. Missing records do not invent completion.

These checks do not fence older unconditional writers or skip diagnostics shared
by different history scopes. Status reads counts, freshness and the coverage
marker in one read-only snapshot. A historical `full` is displayed as `partial`
while retained API thread work or incomplete API history remains. Reads preserve
the raw marker and timestamp; clearing work reveals historical full only when
that marker already existed. Genuine partial, absent and other marker values are
not promoted. Malformed retained history rejects a full projection; other marker
values stay lazily checked unless Doctor needs explicit retained facts.

Sync now checks those archive-wide facts and publishes coverage in one writer
transaction, together with any eligible workspace-only skip cleanup. If retained
work blocks a locally full-eligible run, the previous marker and timestamp stay
unchanged. Genuine partial results write partial. Validation or write failure
rolls cleanup and publication back together. Workspace completion is still
separate, and repair remains workspace-scoped and partial-only. These are
snapshot guarantees, not newest-invocation ordering or continuously current raw
markers; missing local progress does not prove completed Slack history.

Git-share snapshots retain manifest version 1 and their existing table format,
but these API coverage checkpoints are local-only and are not exported. Imports
consume and validate older snapshots containing them without applying them:
merge preserves the destination's own checkpoints, while explicit restore clears
coverage and leaves it unknown. Data-only snapshots do not back up API completion
state.

Before the first writable open, stop every old process accessing the database.
Complete a consistent pre-upgrade backup using SQLite's
[Online Backup API](https://sqlite.org/backup.html) and verify that the backup
opens independently with the expected schema and data. Retain the old binary and
configuration alongside the database backup. Copying only a live database's main
file can omit WAL data. Old binaries reject schema v8 on a new open, but that
check does not stop already-open clients. Do not mix old and new running processes.

Recovery with an older binary requires restoring the consistent pre-upgrade
database backup and configuration after stopping the upgraded processes. Account
for writes since that backup: restoring it discards those writes unless a
separately reviewed recovery preserves them. Do not lower `user_version` or
attempt an in-place downgrade. A data-only snapshot readable by an older binary
does not make old-version syncing or opening an upgraded database safe.

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
- `users` and `channels` return 100 rows by default and accept a positive `--limit` override
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

Automatic Codex credentials are restricted to the HTTPS `chatgpt.com` origin,
including redirects. A custom HTTP MCP origin requires a dedicated `token_env`
other than the Codex fallback variables. It never falls back to Codex env/file
credentials when that dedicated variable is missing. The default Codex account
ID is not sent to custom origins; configure a dedicated `account_id_env` when
the custom server needs one. Explicit custom-server tokens and stdio remain
supported.

`max_pages` bounds the text connector's users, channels, channel-history, and thread pagination loops and the native reference adapter's users/channels loops; hitting the bound returns an error instead of silently accepting an incomplete page set. Native history/replies tools do not accept pagination arguments. The Codex HTTP connector accepts at most 20 channel or user search results per request. With `include_dms` omitted/true, explicit channel IDs avoid global channel and user enumeration. Normal MCP sync retries the pending checkpoint interval or overlaps the completed history-response watermark by one hour, applying current retention; stored message maxima do not establish coverage. It also rechecks persisted thread roots because Slack does not move an old root into channel history when it receives a new reply. `--full` removes incremental bounds, while `--latest-only` uses retained non-draft history or retention-seed eligibility. MCP is an explicit source and is not included in `--source all`.

The text connector's channel search response may omit privacy metadata. With `include_dms` omitted/true, those channels remain locally searchable with kind `mcp_channel`, recording unknown classification. Legacy `publish` still includes these archive rows; that kind is not an export privacy filter.

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

### Native response coverage

Under every DM policy, native history and replies require `ok=true` before
processing that response. A missing or false value stops the sync; earlier
committed batches remain. The text connector's response contract is unchanged.

Native channel catalogs require a `channels` array, user catalogs a `members`
array, and history/replies a `messages` array after the existing decode, error
and success checks. Missing or null collections leave that page uncertified;
they cannot complete empty history or retire a thread job. Explicit `[]` remains
valid, including empty replies. This archive-certification requirement does not
claim that Slack defines every omitted/null collection as invalid. The existing
strict-catalog success check and default-catalog/users OK policies stay unchanged.
Rejected pages preserve earlier commits and retry work; corrected responses can
complete a later retry.

Native `has_more=true` or a nonblank `response_metadata.next_cursor` reports
additional pages. The reference tools cannot request those pages. Slacrawl
processes valid fetched messages and later selected conversations, then returns
an incomplete-history error instead of printing completion or advancing the
successful MCP workspace sync record. Use `--source api` for paginated backfill.
The flag survives local timestamp filtering and empty thread results; a later
successful response cannot clear it. Concrete request, identity, or storage
errors take precedence.

`is_limited=true` reports a separate Slack history/message limit. Slack documents
this flag for earlier messages beyond a free workspace's message limit; it does
not detect every access or retention restriction. Slacrawl preserves the same
successful-sync record and asks the operator to review workspace history
availability. API pagination cannot restore messages Slack does not expose.

These checks preserve valid writes, not whole-sync atomicity. Channel metadata
and valid messages may change even when the old successful workspace sync record
remains. The history checkpoint stays pending when history is incomplete; a
completed history scan remains complete if only replies fail. No opaque server
cursor is stored or printed, and these checks do not establish complete archive
coverage or extend the native server's bounded history window. See Slack's
[history](https://docs.slack.dev/reference/methods/conversations.history/) and
[replies](https://docs.slack.dev/reference/methods/conversations.replies/) contracts.

### MCP history checkpoints

MCP incremental bounds come from completed channel-history scans, not the newest
stored message. A newer reply or API/Desktop row cannot move the MCP history
cutoff. Each checkpoint belongs to a workspace, channel, discovered adapter and
normalized `--since` scope. History timestamps must be finite numbers, and the
latest raw history timestamp is recorded even if local filtering or a richer
stored row prevents that message from being written.

The first selected scan after upgrade, snapshot import into a fresh archive, or
whole-snapshot restore has no local MCP history checkpoint. It starts without an
incremental bound, subject to the current purge floor. This may read more history
than earlier versions. `--latest-only` still selects channels with a non-draft
stored message or a retention seed; a checkpoint alone does not select a channel.

If this first text-connector scan needs more than `slack.mcp.max_pages`, repeating
the same command with the same limit reads the same prefix and fails again.
Slacrawl keeps the checkpoint pending and does not write that channel's buffered
history. It does not save an opaque cursor between invocations. Bootstrap one
channel with a temporary, larger **positive** page budget:

1. Record the current `max_pages` in the config used by this sync. Increase it
   under the existing `[slack.mcp]` section, for example from `250` to `500`.
   The larger value must cover the selected history interval; `500` is an
   example, not a guaranteed channel size. It also raises the other MCP page
   limits and can increase memory use, requests and run time.
2. Run `slacrawl sync --source mcp --workspace T01234567 --channels C01234567`
   with that same config. Keep the existing DM policy and retention settings.
   Do not add `--since`: it creates a separate checkpoint and cannot initialize
   the ordinary one. `--full` is not needed and would bypass the purge floor.
3. If the chosen budget is still insufficient, choose a larger budget before
   retrying. After a successful sync, restore the original `max_pages`. Ordinary
   sync then starts from the completed MCP history watermark, subject to overlap
   and retention. A later interval can still exceed that limit and need the same
   recovery.

This is an operator-managed backfill, not automatic continuation across restarts.
An interrupted scan keeps its pending interval; a retry starts that interval
again. API/Desktop rows, API backfill and `--latest-only` eligibility cannot
certify the MCP checkpoint. This procedure does not extend the native reference
server's fixed history window or allow text intake with `include_dms = false`.

A completed empty scan is recorded explicitly and preserves any earlier history
watermark. Later ordinary scans overlap that watermark by one hour. Failed or
incomplete history keeps its original pending interval for retry. Each ordinary
retry reapplies the current purge floor; a prior `--full` does not grant later
retries permission to restore purged messages. Explicit `--since` takes precedence
over `--full` and leaves the ordinary history checkpoint and unselected thread
backlog untouched.

History completion is committed after history writes and before replies. A later
reply or channel failure therefore preserves completed history while keeping
workspace freshness unchanged. A newer attempt supersedes the older history
revision. Checks around each history tools/call discard revoked responses before
parsing or further text pagination. Metadata, thread preparation, message batches
and later history-derived discovery also check ownership in their write
transaction, including empty history outcomes. The older invocation reports a
retryable failure instead of admitting those stale writes.

The matching completed revision still permits discovery after history completion.
Previously committed batches remain; an in-flight call is not canceled and no new
transport retry is added. Workspace/user catalogs, independent queued replies,
no-tool tombstone reconciliation and whole-Sync workspace publication after
completed history are outside this history-write guard. History progress itself
does not advance status freshness.

These local checkpoints are excluded from Git share exports and imports. Merge
preserves the receiver's existing checkpoints; whole-snapshot restore clears them.
Native MCP still makes its existing bounded history request without a wire
`oldest` argument or cursor pagination. Pending intervals cannot recover messages
outside the server's available window; use API backfill where supported.

### Retained MCP threads

With a thread tool, ordinary MCP sync and `--full` without `--since` save
eligible retained roots before history writes can remove their reply hints.
New hints and work identified by archived replies are saved with their message
batch, even if a later batch fails. If admitted history revives a parent after
its work was canceled, that transaction saves a new job without replacing any
existing generation, including work created after this sync prepared. Ordinary
sync drains only jobs it prepared or newly queued; another sync keeps its job.
The local replies queue survives request failures, incomplete native replies and
process restarts; it does not store a server cursor or extend the native server's
history window.

Explicit `--since`, including `--full --since`, fetches threads only for roots
themselves returned in history. A returned root can qualify through an archived
child even when its reply count is absent. A returned child alone does not add
an older, unreturned parent. After history writes, one transaction checks the
current history revision and acquires only eligible returned roots, using final
stored evidence and positive page hints. It renews selected existing MCP jobs;
unselected backlog and API jobs/skips remain untouched. The shared generation
fences older ordinary or scoped replies, including another Since or adapter.
Channel selection and DM admission still determine which conversations can be read.
Since selects roots; their reply requests have no date bound.

During ordinary sync, a connector without a thread tool reconciles selected
stored tombstones after valid history writes, including tombstones merged from
a share. This cleanup neither creates nor renews jobs. If live work remains,
sync returns an actionable error, preserves the old successful workspace record
and requires a connector with thread support before retrying. With no pending
work, the existing behavior remains. Explicit Since without a thread tool does
not acquire replies work or inspect/clean the ordinary queue.

Every reply traversal checks its acquired generation and live parent around
requests and in parent/reply writes, including empty responses. Deletion or
renewal stops stale pagination and discards the response after the in-flight
request returns; it does not immediately cancel that request or undo earlier
committed history or parent writes. A newer history revision alone does not
revoke independently acquired replies. Another writer's renewed work can remain
pending even when the current sync records successful freshness.

Complete replies retire their matching job even if the enclosing history is
incomplete. History coverage still prevents successful workspace freshness;
incomplete replies keep their job. Scoped sync acquires all selected roots before
requesting replies, so an earlier failure leaves even unvisited selected jobs for
a later ordinary retry. Local tombstone, purge and share lifecycle
rules are shared with [retained API work](#retained-api-threads). Neither queue
certifies complete Slack capture or export safety.

### MCP direct-message policy

`[sync].include_dms = false` applies to both `--source mcp` and `connector`.
The text connector cannot establish native conversation types, so this setting
stops it immediately after tool discovery, before data calls or archive writes.
Use `--source api` or a qualified native reference MCP server for this policy.
Tool names, connector metadata, channel names, ID prefixes, and a private flag
alone do not establish conversation type.

For the native reference adapter, strict exclusion reads the complete available
channel catalog once, requiring `ok=true` on every page. Explicit IDs require an
exact catalog ID match. Existing selectors and exclusions apply before admission;
all observations for a selected duplicate ID are checked together. Selected
catalog identity is checked before DM classification under every policy: a
present foreign context workspace stops the sync even on a DM or duplicate.
That record cannot supply type evidence for the configured workspace. Native
`is_im`/`is_mpim` flags exclude that identity even if other channel flags appear.
Other selected conversations require consistent public/private native type
flags; missing or conflicting evidence stops the sync before archive writes,
including with `--latest-only`. Fixed counts report skipped DMs. If no
conversations are eligible, workspace/user rows and MCP freshness are untouched,
and output says so.

Under every policy, available observations for selected IDs survive name matching
and deduplication, including observations from earlier name requests. An excluded
alias excludes its whole ID before identity checks; unrelated catalog IDs are
not admitted or validated. Omitted/true retain existing payload selection and
per-name requests. Direct-ID-only selection still avoids enumeration
under those policies, so it does not acquire catalog evidence.

Under every policy, selected catalog and retained message context workspace IDs
must agree with the configured workspace. Every returned channel page and thread
parent/reply must match the requested conversation and thread before the affected
writes. Native message identities are checked before local timestamp filtering
or conversion. Top-level history messages require finite numeric timestamps;
thread messages require nonblank timestamps, and replies must not reuse the parent
timestamp. Nested metadata and catalog latest
timestamps remain optional. Earlier successful history writes remain if a later
thread fails. Missing context is bound to the operator's configured workspace;
it is not authenticated identity proof. External authors and their team IDs are allowed.

Omitted/true retain the existing request patterns and stored payload projections,
subject to the identity checks above. Previously archived DMs are not
purged. The reference server's bounded history window and MCP cursor-based
coverage limits remain unchanged; admission does not certify complete history,
DM-origin history, or a safe export.

Ctrl-C cancels MCP sync and stops its stdio server, including when the server stops reading requests and fills the stdin pipe.

MCP response failures report the operation, HTTP status or JSON-RPC code when
available, and a fixed reason. Ordinary errors omit server response bodies,
tool names and error text, parser snippets, conversation identifiers, and opaque
cursors. Transport errors retain cancellation/deadline detection and the
credential-origin rejection reason without printing returned URLs. These
diagnostics do not establish conversation scope or remove previously archived
data.

## External Archive Providers

Use `[[providers]]` to adapt another local archive to the canonical SQLite
schema without adding a source-specific integration to `slacrawl` itself.
Each provider is an explicit sync source and is not included in `--source all`.

With `[sync].include_dms = false`, provider v1 sync stops before reading its
checkpoint or launching the adapter. The protocol accepts arbitrary channel
kinds and opaque raw payloads, so it cannot establish DM exclusion. Use API sync
or a supported Slack workspace JSON export with this setting. Omitted/true keep
existing provider behavior, request bytes, and checkpoint scopes.

This gate governs provider intake. The CLI may initialize the archive and check
Git-share freshness first; earlier configuration/share errors keep precedence.
It does not purge existing DMs or certify an archive as safe to export.
`slack.desktop.include_drafts` remains specific to Desktop intake.

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
- snapshots include eligible cached public-channel media by default; table shards are gzip-compressed and media files retain their raw cache layout
- `subscribe` writes a git-reader config, disables Slack API and desktop sources for that config, clones the repo, and imports the snapshot
- pass `--db` to `subscribe` when you want the reader archive to use a non-default SQLite file
- `update` pulls and merges changed snapshots; an unchanged manifest can still restore missing media and refresh successful-import state
- `search`, `messages`, `mentions`, `sql`, `users`, `channels`, `report`, `digest`, `analytics`, and `files` auto-refresh stale git-backed snapshots before querying when `auto_update = true`
- `stale_after` controls how old the last successful import can be before the next read pulls/imports again
- `status` and `doctor` only observe the configured share repo and last import / manifest freshness; they do not auto-refresh

With `[sync].include_dms = false`, legacy Git snapshot imports stop because
their raw table format cannot enforce DM exclusion. This covers subscribe,
update, exact/historical restore, and stale automatic imports before read,
non-Desktop sync, tail, or file-fetch commands. Subscribe rejects before saving
its importing configuration; explicit updates reject before opening the archive
or acquiring Git data. Automatic paths open the archive to check staleness,
then reject before acquisition, snapshot/media import, or freshness updates.

Set `[share].auto_update = false` to continue querying the local archive or
syncing API/native sources while retaining DM exclusion. Fresh automatic reads,
`subscribe --no-import`, and Desktop-only sync/watch remain available.
Omitted/true keeps the existing merge and restore behavior.

Explicit `[sync].include_dms = false` also blocks `publish`, including
`--no-commit`, `--no-media`, and tag/push modes. After flag, config and argument
checks, publishing rejects before archive initialization, media-cache locking,
Git work or snapshot writes. Keep the archive local. Omitted/true preserves
unfiltered private snapshot publishing, including archived DMs and drafts;
intake settings do not purge those rows or certify publication safety. This
gate does not change the optional release notifier before command dispatch.

## Token Sources

Each Slack token source is controlled independently.

Text normalization notes:

- malformed UTF-8 is repaired before indexing
- compatibility forms are normalized with NFKC
- zero-width and non-printable control noise is stripped from indexed text
- weird spacing is collapsed so FTS and mentions stay queryable even when Slack/Desktop payloads are messy

### Bot token

When configured, the bot token is primary for API sync:

- channel discovery
- users snapshot
- channel history
- channel replies when no user token is configured

Bot replies use the same authenticated workspace and `api-bot` rank 2 provenance.
Slack scopes and channel membership still limit access; a successful channel
thread does not establish full workspace thread coverage. Tail repair retains
its user-token reply path.

An invalid configured bot stops sync; it does not fall back to a user token.
Disable it for user-only API sync or desktop-only operation:

```toml
[slack.bot]
enabled = false
token_env = "SLACK_BOT_TOKEN"
```

### App token

Use the app token with a bot token for live Socket Mode tailing. A user token
plus an app token does not satisfy Tail's bot requirement:

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

Without a configured bot, API sync authenticates the user as primary for
workspace validation, channel discovery, profiles and history. This applies to
`api`, its `bot` alias, and the API portion of `all`. User-primary history uses
`api-user` rank 1, never joins channels, and reports ordinary history scope
errors. Its history coverage and successful workspace marker remain separate
from `api-bot`; switching tokens does not borrow the bot's completed interval.

With a bot, the user is optional. Slacrawl prefers it for historical replies and
optional DM discovery while the bot retains primary ownership. When configured,
the user remains the reply credential: rejected authentication or reply access
does not retry with the bot. Disable the user credential to select bot replies.

When both authenticate, bot and user tokens must belong to the same workspace. API sync and
periodic tail repair reject a valid user token from another workspace before
fetching data or updating the archive. Doctor reports that mismatch as
unavailable user auth with partial thread coverage. Missing or invalid user
tokens retain bot-only coverage when a bot is primary. User-primary auth errors
and cancellation stop Sync. CLI archive initialization and automatic share
checks still occur before API authentication.

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

### Doctor coverage ownership

Doctor authenticates a configured user even without a bot and fills the
existing user availability/error fields. Bot credentials remain absent and
Tail remains unavailable without them. Failed user auth is reported as an
unavailable capability rather than a successful user session.

In JSON, `slack_api.thread_coverage` describes global credentials; top-level
`thread_coverage` uses the named-workspace aggregate when present. An archived
`api-user/thread_skip`, pending API thread work or incomplete retained API
history downgrades either full result to partial in both fields.
Individual `workspace_api` diagnostics remain unchanged. Doctor obtains its
Status projection and explicit retained facts from the same archive snapshot;
neither read rewrites the historical coverage marker. Recent channel skips
combine `api-bot` and `api-user`, newest first with channel-ID tie ordering and
one limit of 20.

If retained API work changes global coverage from full to partial, Doctor adds
`slack_api.thread_coverage_reason = "retained_api_thread_work"` and displays
`partial: retained API thread skips or pending work`. History-only incompleteness
uses `retained_api_history_work` with `partial: incomplete API history`; thread
skips/jobs take precedence when both exist. It does not describe a valid user
session as missing. An already-partial global auth result keeps the
authentication explanation, even when retained work also lowers the named
aggregate. JSON omits an unset reason; `--format log` includes
`thread_coverage_reason="-"`. With valid user auth and no recognized reason,
human output reports `partial` without guessing the cause.

### Doctor DM access sampling

Doctor samples at most one available IM and one MPIM history. A non-scope catalog
failure sets optional `dm_probe_error = "catalog_failed"`; a non-scope history failure
sets `"history_failed"` and does not prevent sampling the other kind. Missing
scopes accumulate independently, so scope warnings and a probe failure can appear
together. Global and named workspace output reports both, with a retry suggestion.
The new field contains only those codes, never provider payloads or cursors.
JSON omits an unset field; log output displays `dm_probe_error="-"`.

These checks leave authentication, DM inclusion and thread-coverage decisions
unchanged. `user auth available for replies` describes a capability, not a
completed backfill. `enabled for user token; history coverage not verified` does
not claim that empty catalogs or unsampled kinds passed history checks.
Canceling Doctor or reaching its caller deadline stops subsequent requests and
returns an error without a report. A request timeout while the caller remains
active is a bounded probe failure; unavailable optional auth keeps its existing
fallback behavior.

### Retained API threads

Ordinary API sync and `--full` without `--since` revisit eligible roots already
in the selected channels, even when fetched history no longer contains their
reply hints. Roots need a positive reply count or a distinct archived child;
an empty or self-referencing `thread_ts` is supported. Explicit `--since`,
`--full --since` and Tail repair do not drain this backlog. A committed replies
collision queues or renews its requested owned/live root for ordinary retry;
excluded conversations leave work untouched. Current-page thread processing
keeps its existing scope.

Replies work is saved locally before history can overwrite hints and in the
same transaction as fetched messages that establish roots through reply counts
or retained child relationships. Later history errors, incomplete responses and
intentional scope skips keep that work pending for a later sync. A bot-only API
sync can complete channel replies without claiming full workspace thread coverage.
Unavailable reply credentials or access leave the saved work pending while bot
history can still complete. Bot and user replies share the existing `api-user`
work queue and generation guards; fetched messages retain the actual credential
source. Switching from bot-primary to user-primary sync keeps
that replies work without borrowing the bot's history checkpoint.

Existing message reconciliation can revive a tombstone when an in-flight history
page arrives later. If that page admits thread evidence after cancellation, its
transaction queues fresh work while preserving every existing generation, including
work another sync created after this sync prepared. Ordinary replies requests use
only generations prepared or newly queued by this invocation; a competing sync
keeps ownership of its response and skip state across those history commits.
Roots successfully completed during this sync stay excluded. If a generation
is rejected before any replies request or committed cached skip, a later page
can admit and process fresh work. Revocation after a request still counts as an
attempt and can leave newly queued work for the next sync.

Starting a new ordinary preparation renews selected retained generations, even
without replies capability, and can supersede an earlier replies worker. The
replacement work remains pending with partial coverage. Capability-aware thread
preparation and concurrent-sync fairness remain follow-up work.

Successful replies retire only the generation that was processed. Retained
requests and writes recheck that generation and the parent's live ownership;
deletion or renewal during a request discards its stale response. Bulk retirement
of unrelated legacy thread skips requires Full with no `--since`, no channel
allow-list and no effective channel exclusions, after DM enumeration completes
with DMs enabled and no conversation or message work omitted. Full cleanup also
keeps thread-skip records while API replies work or incomplete API history in
that workspace remains. The history check and pending-thread deletion guard share
one writer transaction; canonical foreign-workspace records do not block cleanup.
Successful individual replies still clear their own skip during scoped runs. Remaining retained work runs after complete history traversal
and before the completed history horizon is saved. A replies failure can therefore
stop later channel or media work while preserving committed messages and the
pending history interval.

DM catalog filtering or missing scope, admission drops, recoverable history skips,
unavailable retained roots, and channel or message ownership collisions keep this
sync's recorded thread coverage partial. A later successful channel or concurrent
thread completion does not erase that omission. Scoped
runs retain unvisited diagnostics; successful scoped replies still clear their
own skips. History or replies collisions keep that channel's exact attempted
interval and prior completed horizon. A collision anywhere in replies pagination
also preserves the job and exact skip; later errors still win, and valid rows on
other pages or channels commit. With no owned generation, that collision queues
or renews only the requested live root, even without reply-count/child evidence.
The queue write follows discovery filtering and commits with the page, so an
older ordinary reply cannot retire the new generation. An ordinary retry can
drain it without fresh history hints; successful scoped replies leave the job.

Catalog-only omissions still belong to the current invocation. CLI Doctor reads
retained history/work when evaluating full coverage; its store-free capability
probe does not certify archived completeness. Static scope restrictions alone
retain the existing scoped coverage behavior. No erased evidence is reconstructed,
and this does not qualify mixed older writers or continuously current raw
coverage markers.

When a retained root returns `thread_not_found`, its job remains pending with
a root-specific skip and partial coverage. Healthy roots and later channels
continue, and the next ordinary sync retries the unavailable root. The error
does not establish deletion. Other reply failures retain their existing behavior.

Committed parent tombstones, message removal through the store, and local purge
cancel matching work and its API thread-skip record, even when the pending job
is already absent. Ordinary preparation also reconciles skips backed by stored
tombstones in the selected workspace and channel, including after a Git-share
merge and without a remaining reply hint. Live or missing parents are not
deletion evidence; malformed pending work still requires repair. History polling
does not discover hidden deletion events; see Slack's
[message contract](https://docs.slack.dev/reference/events/message/#hidden-subtypes).
Missing reply metadata is not deletion. The local jobs are excluded from Git
share export/import and freshness timestamps; archive source entry counts still
include them. A full restore clears local work along with replaced archive rows.
MCP uses the same local work lifecycle with its own
[reply and scope rules](#retained-mcp-threads). This does not certify complete
Slack capture.

### API catalog page success

Channel and user catalogs must report `ok: true` before their rows or cursors
are returned. Missing, null or false success with no concrete Slack error stops
that catalog operation. A failed later page discards its earlier catalog pages;
retry starts the catalog again. Already completed public history survives a later
user or DM catalog failure, while final workspace/Doctor markers and unvisited
legacy thread skips stay unchanged. Existing concrete missing-scope handling
keeps its current skip behavior.

Ordinary catalogs and profiles use the selected primary token. DM discovery
uses the user token; periodic repair retains the bot-owned channel catalog.
Empty successful catalogs remain valid, including the existing second user
fetch when enabled-DM sync receives an empty user result.

Catalog responses now use the same whole-body reader as history/replies, rejecting
trailing JSON and read errors after a valid object. Non-200 responses use SDK
status errors; bounded retries remain limited to rate limits with Retry-After.
Successful catalog pages must also contain a `channels` or `members` array.
Missing/null collections leave the page uncertified; nonarrays fail decoding.
Explicit `[]` remains valid, including on pages with a continuation cursor. This
archive-completeness rule does not label all omitted/null Slack responses invalid
or certify the rest of their payload shape. Doctor probe-error handling is unchanged.

### API authentication, lookups and joins

`auth.test`, `conversations.info` and `conversations.join` must report `ok: true`
before slacrawl uses their decoded results. Missing, null or false success with
no concrete Slack error now fails with a method-specific error. These methods
also reject trailing JSON and read errors after an otherwise valid object.
Concrete Slack errors, typed HTTP status errors and bounded rate-limit retries
keep their existing handling. Success-flag validation does not certify workspace
identity or the rest of the payload shape.

A failed primary authentication stops sync before archive writes; a configured
bot never falls back to the user token on failure. Failed optional user auth
still allows bot history, with replies unavailable. Doctor returns bot-auth
failures and reports user-auth failures as unavailable. Tail rejects failed bot
auth before starting Socket Mode, and an unsuccessful lookup for an untyped
event stops tailing without writes or acknowledgement. A failed join remains a
recorded, nonfatal history skip: sync can finish with partial coverage, but does
not retry history as though the join succeeded. Doctor reports non-scope
capability-probe failures separately from missing scopes.

Workspace-bound operations require `auth.test` to identify a nonblank workspace
after trimming whitespace. A configured or requested workspace cannot replace
that identity. A successful response with an unbound primary identity stops sync
or Tail; an unbound optional user identity stops sync and repair before writes.
Concrete optional-user authentication errors still allow bot-only history.
Doctor rejects an unbound bot identity, reports an unbound user as unavailable,
and uses the canonical user workspace ID for DM probing.

Use a workspace-scoped bot or user token. An enterprise ID may accompany a valid
workspace ID but cannot replace it; organization-token workspace resolution is
not supported. The existing `users.list` request leaves `team_id` empty.

### API request diagnostics

Native request/response failures report a fixed operation and phase, with numeric
HTTP status when relevant. Endpoint URLs, transport/read error text, invalid
Retry-After values and SDK decode snippets are omitted from these rendered errors.
This also protects optional-auth Doctor JSON, failed-join state and progress logs
when they report these failures. Existing retry and partial-work behavior stays
unchanged; the diagnostic no longer includes the underlying failure detail.

For decoded unsuccessful responses, these exact native codes remain readable:
`missing_scope`, `not_in_channel`, `channel_not_found`, `invalid_auth`,
`not_authed`, `account_inactive`, `token_expired`, `token_revoked`,
`is_archived` and `thread_not_found`. Other error strings produce `slack <method> API response failed`;
whitespace, case changes or added text do not qualify. Repeated channel, DM, user,
history and replies cursors report the method without reflecting the cursor.
The SDK's explicit-success error-field bypass, exact skip classification, retries
and pending-work behavior remain unchanged.

Causes remain available to code using `errors.Is`, `errors.As` or unwrapping and
may still contain private native codes, details or metadata. This is not redaction
of error objects, identities, progress names, successful metadata or archive data,
previously stored diagnostics, or every diagnostic surface.

### API history completeness

Native history and replies pages must report `ok: true` before any message on
that page is admitted. Missing, null or false success with blank or absent error
text stops sync or repair with a method-specific error. Concrete Slack errors
keep their existing type and details; previously committed pages survive, and
the pending interval remains available for a corrected retry.

Successful history/replies pages must contain a `messages` array. Missing/null
collections leave the page uncertified instead of completing an empty interval;
explicit `[]` remains valid. Earlier writes, completed horizons and pending work
retain their existing retry behavior. The array requirement also applies to
one-message capability probes; Doctor reports non-scope probe failures separately
from missing scopes. Those diagnostics do not certify complete history.
Authentication, conversation-info and join responses are outside this page-only
check.

API sync and periodic tail repair follow every nonempty history/replies cursor,
even when a page is short or empty. After writing a valid page and handling its
scheduled threads, a terminal `has_more = true` without a continuation cursor
stops the scan with an error. slacrawl does not support timestamp pagination
to continue without a cursor. Unchanged responses leave the same interval
pending on retry.

History `is_limited = true` is retained across accessible pages. Once those
pages have been traversed, the scan reports that completeness of the requested
interval is uncertified. Slack defines this flag for earlier messages beyond
the free-workspace message limit. It does not establish that a particular
bounded interval is missing messages or detect every access/retention limit;
narrowing `--since` is not a completeness bypass.

These checks apply under every DM policy. Valid fetched rows remain stored,
but the previous coverage `Latest` and `Complete` stay unchanged, the attempted
lower bound remains in `Pending`, and ordinary workspace success does not
advance. A corrected retry resumes that pending interval and can complete it.
Concrete request, validation, write, and cancellation failures keep their
precedence; native transport/decode failures use the bounded diagnostics above.
One-message capability probes require successful page responses but remain
independent of scan completion. Doctor reports non-scope probe failures without
changing authentication or thread-coverage decisions.

See Slack's [history contract](https://docs.slack.dev/reference/methods/conversations.history/#message-types),
[replies pagination](https://docs.slack.dev/reference/methods/conversations.replies/#pagination),
and [cursor pagination guidance](https://docs.slack.dev/apis/web-api/pagination/).

### API and tail direct-message policy

For API sync, `[sync].include_dms` defaults to whether a user token is configured.
Set it to `false` to exclude IM and MPIM conversations before channel metadata,
history checkpoints, or messages are written.
Explicit `is_im`/`is_mpim` flags take exclusion precedence even when other
channel flags are present.
For other selected conversations, missing or conflicting native channel flags
fail the sync instead of being treated as complete.
API sync channel allow-lists and excluded names still apply before this check.
Periodic tail repair lists public/private channels and shares this explicit
exclusion check, but does not discover DMs or carry API sync channel selectors.

All API policies reject missing conversation IDs, conflicting context workspace
IDs, and mismatched channel IDs in typed latest-message, history, or reply
payloads. A rejected page is not written; earlier pages and unfinished coverage
remain available for retry. Slack Connect authors and conversation hosts may
belong to other workspaces and do not trigger this check.

API history/replies and periodic repair also reject empty or whitespace-only
top-level message timestamps before writing any part of that page. Accepted
timestamps stay unchanged; nested metadata and catalog latest timestamps remain
optional, and native replies may echo their parent. Failure preserves earlier
pages and the previous successful workspace state; retry resumes the pending
history interval.

For live Socket Mode events, omitted/true keeps accepting delivered DMs even
without a user token. Explicit `false` skips `im`, `mpim`, and the retired
workspace-app `app_home` DM event type before content normalization or writes,
then acknowledges the intentional skip. Native `channel` and `group` message
events need no extra request. Missing or unrecognized types, including ordinary
edit/delete envelopes, and rename/archive/unarchive events require a fresh
[`conversations.info`](https://docs.slack.dev/reference/methods/conversations.info/)
lookup with the bot token. Grant the relevant `channels:read`, `groups:read`,
`im:read`, or `mpim:read` access. Lookup failures and identity mismatches stop
tailing without acknowledging that event. After identity validation, explicit
`is_im`/`is_mpim` flags cause an intentional skip and ACK even when other channel
flags are present. Otherwise, unknown/conflicting channel flags stop tailing
without ACK; correct the access/configuration problem before restarting.

These lookups run before ACK and use the existing rate-limit retry behavior.
They can delay ACKs beyond Slack's delivery deadline; no prompt-ACK guarantee
is made for events requiring lookup. Results are not cached, and the returned
conversation and its latest message are not stored. Channel metadata updates
only affect existing rows in the authenticated workspace; missing or foreign
rows are intentional no-ops under every policy. Retained message channel IDs
must agree with the envelope, including nested edited/deleted/root messages.
Slack Connect event/author workspace IDs and differing event/message timestamps
are not conversation identity conflicts.

This controls future API/tail/desktop/MCP and Slack-export intake and rejects
legacy Git share imports and external provider v1 sync. It does not purge
archived DMs. Desktop uses the policy as described below; MCP uses the native
evidence requirements above. It does not certify the archive or a Git share as safe to publish:
admitted messages can contain sensitive text
and file metadata, and a current channel type does not establish that its
history lacks messages from a converted group DM.

## Slack Export Admission

`[sync].include_dms = false` applies to import, including `--dry-run` and `--force`.
Omitted/true preserve DM inclusion. Strict admission supports a declared Slack
workspace JSON export with `channels.json`, `groups.json`, `dms.json` and/or
`mpims.json` reference catalogs. A present empty array is valid; null catalogs are invalid. Unrelated
directories/ZIPs without those catalogs are unsupported. Single-user/TXT and
other layouts need separate qualification.

DM catalogs or positive native `is_im`/`is_mpim` observations exclude every
occurrence of the same conversation ID. Their message bodies are not decoded.
For non-DM catalogs, if all four discriminator keys (`is_channel`, `is_group`,
`is_im`, `is_mpim`) are absent, the catalog supplies public/private type and any
supplied `is_private` must agree. If any discriminator is present, sparse
fallback is disabled: positive native channel flags must establish a public or
private channel. Catalog privacy fills only an absent `is_private`; explicit
native privacy wins. Negative-only, all-false, contradictory, null or malformed
flags cannot establish admission. Recognized native flag names are matched
case-insensitively for classification and the DM veto. Names and ID prefixes
never establish type.

Strict catalogs and admitted message JSON reject repeated decoded object keys
before projection, including nested objects and escaped duplicate keys. Catalog
ID/name and native type/privacy keys also reject case-insensitive duplicates.
Omitted/true keep
the existing JSON decoder behavior.

All catalog IDs and name/ID candidates remain reserved, including excluded and
unused fallback candidates. Directory imports compare device/inode identity on
Linux and macOS; strict directory admission on other platforms requires a ZIP
instead. Contained aliases are allowed only for the same conversation. ZIPs
retain original entries and frozen local-header metadata and reject ambiguous
logical names or cross-conversation payload reuse. Payloads still use the
standard ZIP checksum/decompression path.

Retained raw conversation IDs and `context_team_id` must match their prepared
channel/workspace before projection or timestamp/priority skips. This includes
every case-insensitive occurrence of recognized identity fields and message,
previous_message, root, previous and catalog-only latest objects. External author teams, mentions and file-sharing references are not
enclosing conversation identity. Missing channel identity inherits the catalog.

After body validation, imports check existing workspace ownership for admitted
channel IDs and every retained user ID before workspace, user or channel writes.
Dry-run performs the same checks. Store write-time checks remain in place;
the preflight does not make concurrent imports or the whole import atomic.

The first body scan fixes the name-first/ID-fallback choice and records each
file's identity and digest. Any raw row, even one without a timestamp, selects
the name branch; only zero rows permit ID fallback. The write pass verifies and
decodes the same opened-file buffer. New files/catalog edits wait for a new
preparation; missing, replaced or changed planned files stop execution.
Earlier 500-message commits survive a later failure; the pending remainder
does not commit.

Strict all-excluded imports do not initialize the archive/runtime directories
or write workspace/users. Dry-run opens an existing database read-only, treats
a missing one as empty, and never migrates or repairs it. Read-only errors stay
visible. These promises concern importer archive/runtime initialization, not
the CLI's independent interactive release notice or SQLite WAL sidecar bytes.

Fixed omission counts report excluded conversations. Already archived rows
remain, independent user profiles remain eligible, and import retention/priority
semantics are unchanged. Catalog evidence does not authenticate the producer,
prove complete capture, or establish that a current channel never originated
as a group DM. This is not a public-export minimizer or a safe-to-publish verdict.

## Desktop Source

Desktop ingestion is optional and read-only.

`[sync].include_dms = false` also gates desktop conversation-derived writes.
Omitted/true preserve desktop recovery defaults. Strict exclusion requires
selected public/private channel metadata and omits unknown, conflicting, or
heuristic records; every draft destination must be eligible. Fixed omission
counts explain partial intake. Existing archived rows, independent profiles,
and custom statuses remain. See [Desktop DM exclusion](desktop-mode.md#excluding-direct-messages)
for classification, decode coverage, and retained-history limits.

Sent Redux messages honor current purge retention floors at batch write time
across desktop/wiretap, watch, and all/hybrid. Metadata and admission counts may
still refresh; drafts have separate retention behavior. See
[Retention after purge](desktop-mode.md#retention-after-purge).

New read-marker checkpoints use workspace/channel tuple keys in
`desktop/read_marker_v1`; legacy channel-only rows remain untouched.
See [Read-marker checkpoint identity](desktop-mode.md#read-marker-checkpoint-identity)
for scalar values, user/call aggregation and snapshot compatibility.

```toml
[slack.desktop]
enabled = true
path = ""
```

Behavior:

- `enabled = true` turns on desktop sync support
- `path = ""` auto-detects the supported macOS or Linux Slack Desktop path
- `path = "/custom/path"` overrides detection
- `include_drafts` defaults to `true`; set it to `false` to exclude unsent drafts
  from desktop/wiretap sync, `watch`, and the desktop phase of `sync --source all`
  or `hybrid`

```toml
[slack.desktop]
enabled = true
include_drafts = false
```

Excluded drafts do not create messages, channel hints, event history, search
entries, or draft counts in sync output. Desktop snapshots still read the local
cache. This setting does not delete drafts already archived, filter DMs, or
sanitize Git snapshots; use a separate empty database when starting an archive
that must never contain drafts.

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
