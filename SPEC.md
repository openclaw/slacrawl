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
- files: metadata in SQLite, opt-in blob downloads in the local cache
- Git sharing includes eligible cached public-channel media in its raw cache layout; a future compressed-media format must retain raw-media import compatibility
- desktop-local source: supported Slack Desktop cache paths on macOS and Linux

## Local Environment Contract

An agent should assume:

- shell: `zsh`
- Go `1.27.0+` is installed; the preferred build toolchain is `1.27.1`
- desktop-local Slack data may exist under:
  - `~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack`
  - `${XDG_CONFIG_HOME}/Slack`
  - `~/.config/Slack`

## Slack Data Model Notes

Important Slack facts that drive the schema:

- messages are scoped by `(channel_id, ts)`
- threads remain message relationships via `thread_ts`
- Slacrawl's historical thread-reply path uses a user token for public/private channels
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
- `message_files`
- `message_events`
- `message_event_heads`
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
- if a configured user token authenticates successfully, matching the bot's workspace when a bot is present
- recent `api-bot` and `api-user` channel skips together, ordered by `updated_at DESC, entity_id ASC` with one limit of 20; retain the opened-empty array shape
- tail connection/repair state when present; Tail requires bot and app credentials even with a valid user token
- configured git-share repo plus last import / stale state when share mode is enabled

Nested `slack_api.thread_coverage` belongs to global-token diagnostics. Top-level
`thread_coverage` uses the named-workspace aggregate when configured. Retained
`api-user/thread_skip` rows, pending API thread work or incomplete retained API
history downgrade either full result to partial in both fields, without changing
individual workspace diagnostics or the persisted coverage marker. Doctor reads
Status and these retained facts from the same archive snapshot.

When retained work changes global coverage from full to partial, Doctor sets
optional `slack_api.thread_coverage_reason` to `retained_api_thread_work` when
skips or thread jobs exist; history-only incompleteness uses
`retained_api_history_work` and renders `partial: incomplete API history`.
Human output names the retained API skips or pending work instead of reporting
missing user authentication. An already-partial global auth diagnosis keeps its
original meaning; valid user auth without a recognized reason renders neutral
`partial`. JSON omits an unset reason; log output includes it as `"-"`.

Doctor reports DM access-sampling failures separately from missing scopes. Optional
`dm_probe_error` is `catalog_failed` when DM enumeration fails for a reason other
than `missing_scope`, or `history_failed` for a non-scope sampled history failure.
History sampling continues with the other available IM/MPIM kind; later success
does not erase a failure. Missing scopes remain sorted and independent. The new
field contains no raw provider errors, cursors, conversation IDs or URLs. JSON
omits an unset field; log output includes `dm_probe_error="-"`.

Probe failures do not change user authentication, DM inclusion or thread-coverage
fields. Human output names failures for global and named workspaces and offers a
Doctor retry. Full thread capability reads `user auth available for replies`;
DM inclusion reads `enabled for user token; history coverage not verified`.
Empty catalogs and unsampled kinds do not establish history access or archive
completeness. Caller cancellation/deadline stops Doctor, remaining requests and
CLI output; a request error resembling cancellation with a live caller remains
an optional-auth failure or bounded probe failure.

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

### `sql`

- Accept one read-only `SELECT` or `WITH ... SELECT`, including leading comments.
- Match SQLite token boundaries for strings, quoted identifiers, named-parameter suffixes, and comments; only LF ends a line comment. Reject additional statements before execution while retaining SQLite `query_only` enforcement.
- Return rows keyed by their exact column names. Reject duplicate names, including for empty results, rather than overwrite a value; callers can supply unique `AS` aliases. Case-distinct names remain distinct keys.

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
- preserve delivered DM events when `include_dms` is omitted/true; explicit false skips native IM/MPIM/app_home messages before normalization or persistence and acknowledges the skip
- under explicit false, resolve missing/unknown message types and channel metadata events with uncached bot `conversations.info`; require exact ID, matching explicit context workspace, and a known native type; failed/unknown lookups return an error without ACK
- validate retained nested message channel IDs before normalization under every policy; Slack Connect outer/author workspace IDs and differing event/message timestamps are not conflicts
- restrict rename/archive/unarchive writes by workspace and channel ID; missing/foreign rows are intentional no-ops
- lookups and rate-limit retries precede ACK and can delay it; current channel type is not proof of DM-free historical content

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
- user token: `xoxp-`; optional beside a bot, or primary for user-only API sync
- each token source can be enabled or disabled independently
- desktop source can be enabled or disabled independently
- `[slack.desktop].include_drafts` defaults to `true`; explicit `false` excludes
  draft-derived state from desktop/wiretap sync, watch, and all/hybrid sync
  before persistence, without deleting drafts already archived
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
- explicit `[sync].include_dms = false` rejects legacy Git snapshot imports because the format has no DM admission evidence; omitted/true preserve import behavior
- subscribe checks this policy before saving an importing configuration; update/restore check before opening the archive or acquiring Git data
- automatic paths first open the archive to check staleness, then reject before Git acquisition, snapshot/media import, or successful-import state changes
- `subscribe --no-import`, fresh automatic reads, `auto_update = false`, and observation-only status/doctor remain available
- explicit `[sync].include_dms = false` also rejects legacy snapshot publishing; the shared export owner checks before cache locking, Git work, store access or snapshot writes, and the CLI checks after flag/config/argument validation but before archive initialization or tag validation
- publication rejection leaves the archive local; `--no-commit`, `--no-media`, and tag/push options cannot bypass it. Omitted/true preserve unfiltered private snapshots, including archived DMs and drafts; neither intake policy purges stored rows nor certifies publication safety
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
3. select the configured bot, otherwise the user, as primary for this invocation; authenticate it and reject failures without fallback. Validate the workspace and any successfully authenticated optional user before API data writes
4. fetch workspace metadata
5. fetch channels, apply allow-list and excluded-name filters, then admit conversations before metadata or coverage writes:
   - explicit `[sync].include_dms = false` skips IM/MPIM and rejects unknown or conflicting conversation types
   - omitted/true retain the existing per-source acquisition defaults
   - reject missing channel IDs, foreign context workspace IDs, and mismatched typed latest-message channel IDs for retained conversations under every policy
6. select channels and explicit history mode; acquire the actual window atomically when the channel scan begins:
   - explicit `--since` wins
   - `--full` disables incremental cutoffs
   - `--latest-only` skips channels that do not already have a stored cursor
   - otherwise reuse the last completed source/workspace/channel history horizon with overlap
   - retain unfinished intervals across failures; observed message maxima do not certify completed backfill
   - histories without a completion checkpoint start at the permitted retention floor, including desktop-only or legacy archives
   - explicit `--since` coverage is isolated from ordinary/full history checkpoints
   - validate the selected canonical checkpoint and channel ownership, then acquire a random generation with the actual lower bound and requested upper horizon in one Store writer transaction
   - choose the upper horizon as the maximum of the supplied clock, completed horizon and prior active request horizon; completion consumes that recorded bound, including for empty history
7. persist admitted channel metadata
8. fetch users with the primary client, including the profile snapshot used for DM names
9. backfill message history
10. for bot-primary history only, when `auto_join` is enabled, attempt public-channel join and retry once on `not_in_channel`; user-primary history never joins and does not suppress ordinary `missing_scope` failures
11. backfill thread replies when the user token authenticates to the selected workspace; reuse primary user authentication without another auth call
    - ordinary sync with empty `Since`, including Full, durably queues eligible retained roots before history and page-discovered roots with their message transaction; post-history discovery runs after completeness checks
    - starting ordinary preparation renews selected retained generations even without replies capability; it can supersede earlier work, which remains durably pending with partial coverage
    - use `api-user`-owned workspace/channel/root jobs with generation-conditional completion; preserve jobs when hints disappear, replies fail or replies capability is unavailable
    - retain `thread_not_found` jobs with a generation-guarded root-local skip and partial coverage, without blocking healthy roots or later channels or inferring deletion
    - reconcile retained work before completed history coverage; do not re-enqueue roots completed during the same attempt
    - recheck every scoped candidate in the history write transaction: admitted revival after cancellation queues fresh work, all extant generations remain unchanged, and revoked completion does not exclude later work
    - ordinary replies require a generation prepared or newly queued by this invocation; an unowned page hint does not claim another sync's work or prevent a later page from acquiring canceled work
    - deduplicate only after a replies attempt starts or a cached skip commits; initial generation rejection leaves later admitted work eligible, while revocation after a request still consumes the attempt
    - explicit Since, Full+Since and Tail repair do not drain ordinary jobs; a committed replies collision queues or renews the requested owned/live root for ordinary retry. Excluded conversations leave jobs untouched
    - committed stored tombstones (nonempty `deleted_ts` or `subtype=message_deleted`) and purge cancel matching jobs; preparation reconciles already-stored tombstones before fetching
    - hidden deletion events are not returned by Slack history polling; do not infer deletion from absent hints or claim polling discovers deletions
    - pending jobs stay local across Git share, do not advance freshness timestamps, and keep API Doctor thread coverage partial; MCP consumption remains a separate change
    - capability-aware thread preparation and concurrent-sync fairness remain a separate follow-up; history-commit preservation does not change preparation renewal
12. validate every message channel ID in the complete history/replies page, including nested message, previous-message, and root fields, before normalizing or writing that page; earlier pages remain resumable on failure
   - require explicit native `ok: true` and a present `messages` array before converting history/replies messages; preserve concrete Slack errors, and reject missing/null/false success with blank or absent error text without admitting that page
   - missing message channel IDs inherit the requested conversation
   - after identity validation, require a nonblank top-level timestamp under every policy, including periodic repair; preserve accepted timestamp bytes
   - nested metadata and catalog latest-message timestamps remain optional; native replies may echo the requested parent timestamp
   - repair malformed UTF-8 before indexing
   - normalize indexed text with NFKC
   - strip zero-width and non-printable control noise
   - collapse odd whitespace for stable FTS / mention extraction
13. upsert canonical rows
14. update FTS rows and mentions
15. follow every nonempty history/replies cursor, including short or empty pages
    - after valid page writes and scheduled thread work, reject terminal `has_more = true` without a continuation cursor
    - retain history `is_limited = true` across accessible pages and report requested-interval completeness as uncertified after traversal; this does not prove that a particular bounded interval has missing rows
    - concrete request, decoding, identity, timestamp, store, and thread failures retain precedence
16. write successful coverage only after these checks; failures preserve valid writes, previous `Latest`/`Complete`, and attempted `Pending`, without advancing ordinary workspace success
    - a corrected retry resumes the pending interval before clearing it
    - periodic repair uses the same scan completion rules; one-message capability probes require page success but do not certify scan completion or change which errors Doctor reports
    - existing channel skips and join attempts remain separately recorded
    - bulk retirement of legacy API thread skips requires Full with no Since, no channel allow-list and no effective exclusions, plus completed DM enumeration and no observed omissions; validate workspace history and apply the existing pending-thread guard in one writer transaction
    - DM catalog filtering or missing scope, admission drops, recoverable history skips, unavailable retained roots, and channel/history/reply collisions prevent this Sync from claiming full thread coverage; only complete individual replies retire their work and skip
    - history/reply collisions retain that channel's attempted Pending, Generation and PendingLatest without advancing its completed Latest. Other eligible channels and valid page rows still proceed; later concrete failures retain precedence
    - static scope restrictions alone do not redefine other scoped coverage behavior. Catalog-only omissions remain invocation-local; existing missing checkpoints do not fabricate successful coverage
    - primary history uses `api-bot` rank 2 or `api-user` rank 1; workspace success records that source, coverage remains source-specific, and ordinary message reconciliation is unchanged
    - Tail and periodic repair keep their bot-owned catalog/history path; selecting a primary for Sync never reassigns the stored bot client

API history attempt ownership is local to the exact source, workspace, channel
and normalized Since key. Every physical history/replies request checks caller
cancellation and then ownership before and after the request, including retries.
Page writes, thread preparation/discovery/completion, tombstone retirement,
thread-skip changes and channel skip/join records check the same generation in
their write transaction. A superseded attempt returns a failure and cannot count
as successful channel/workspace completion. Previously committed pages remain.
Join checks do not undo an external join already dispatched; retry sleeps keep
their existing cancellation behavior.

Ordinary acquisition uses current pending work or completed overlap and then
applies the current retention floor; it does not inherit an older Full request's
restore permission. Since still precedes Full, and Full precedes LatestOnly.
Valid legacy v8 checkpoints without generation/upper fields acquire both on the
next attempt without inventing an earlier upper horizon. Malformed selected
canonical values fail without writes. Acquisition checks only the selected key;
it does not fence older unconditional writers or cross-scope skip diagnostics.
API Latest is a completed requested horizon, unlike MCP's returned-message
watermark.

Coverage evaluation scans raw records for the exact API sources and history type
once per decision. Every key must be canonical, even for a different workspace;
after key validation, workspace-only decisions ignore foreign values. Archive-wide
Sync and CLI Doctor evaluate every API value, while repair and Full skip cleanup
evaluate the selected workspace. Malformed selected values fail without clearing
state. A valid record is incomplete when Complete is false or Pending is present,
including an empty Pending; missing records are not invented. Finding pending
work does not bypass validation of later records.

A replies collision without an owned thread generation queues or renews only the
requested live root in that page transaction, after ordinary discovery filtering.
It invalidates older ordinary work; an already guarded reply keeps its generation.
Collisions do not retire the root's job or exact skip. No collision, foreign/missing
roots and final tombstones do not enqueue this conditional work. An ordinary retry
can recover it without a new reply hint.

Status reads counts, freshness, the historical coverage marker and any required
retained API facts in one read-only transaction. A stored literal `full` becomes
`partial` in that snapshot while API thread skips/jobs or incomplete API history
remain. Reads never rewrite the marker or its timestamp. Clearing retained work
can reveal an existing historical `full`, but does not promote a genuine
`partial`, absent marker or other stored value. Malformed relevant history
rejects a `full` projection; other stored values retain lazy validation unless
Doctor explicitly needs the retained facts for a live full-coverage decision.
The public Status JSON shape does not change.

Sync combines eligible workspace skip cleanup, archive-wide history validation,
global API thread checks and coverage publication in one writer transaction.
A locally full-eligible run blocked by retained work leaves the previous marker
and timestamp untouched; a genuine partial result writes `partial`. Errors roll
back cleanup and publication together. Full cleanup eligibility is unchanged,
and workspace completion remains a separate write. Repair stays workspace-scoped
and can only write partial coverage.

These decisions describe the observed snapshot, not newest-invocation ownership,
continuous truth of the raw marker or live Slack completeness. Missing retained
records do not establish completion; sharing or Restore may omit or clear local
progress. No erased evidence is reconstructed. Mixed older writers, catalog-only
collision lifecycle, MCP history writes and Tail lifecycle remain outside this
boundary.

Channel and user catalog pages also require explicit native success before rows
or cursors return to their callers. Ordinary catalogs and users use the selected
primary token; DM discovery uses the user token and repair keeps the bot token.
Missing/null/false success without a concrete Slack error discards the current
catalog operation, including earlier catalog pages. Previously completed public
history survives a later user/DM catalog failure, while final workspace/Doctor
markers and unvisited legacy skips remain unchanged until a corrected retry succeeds.
Existing concrete missing-scope handling remains unchanged.

Catalogs share the history/replies whole-body reader: trailing JSON and a read
error after a valid object are rejected. Non-200 responses use the SDK's typed
status error; only rate limits with Retry-After use the existing bounded retry
policy. Typed catalog decoding still precedes success validation; this does not
certify complete payload shape.

After native success, page admission requires a present array: `messages` for
history/replies, `channels` for conversation catalogs, and `members` for user
catalogs. Missing or null collections leave the page uncertified; nonarrays fail
typed decoding. This is an archive-completeness requirement, not a claim that
Slack defines every omitted/null collection as invalid. Explicit `[]` remains a
valid empty page, and available cursors still continue pagination. Empty user
catalogs retain the existing nil accumulator and enabled-DM second fetch.

Earlier valid history/replies writes and pending work survive collection failure;
catalog failures discard that catalog operation's collected pages. Corrected
retries use the existing pending interval or restart the catalog. One-message
capability probes also require arrays, without certifying scan completion.
Doctor reports non-scope probe failures through its bounded access-sampling
field. Authentication, info and join responses do not use this page-collection
gate. The SDK's existing `ok: true` error short-circuit is unchanged.

Authentication, conversation-info lookups and join attempts use the same native
response owner. Each must report `ok: true` before its decoded result can be used;
concrete Slack errors retain precedence over a missing/false success flag. These
methods also reject trailing JSON and read errors after a valid object, retain
typed HTTP status errors, and retry only rate limits with Retry-After. Typed
payload decoding precedes success validation; success alone does not qualify
workspace identity or other payload shape.

Sync authenticates its selected primary token before archive writes, without
falling back from a failed configured bot to a user token. Invalid optional user
auth still permits bot history with partial reply coverage. Doctor treats failed
bot auth as fatal and failed user auth as unavailable; Tail authenticates its bot
before constructing Socket Mode. Untyped Tail lookups must succeed before type
admission, writes or acknowledgement. Failed joins remain recorded, nonfatal
history skips; only a successful join permits the history retry. Auth response
headers remain private cloned metadata, excluded from archived JSON. Socket Mode
still owns the bot SDK client; HTTP operations use explicit token strings.

Workspace-bound API operations also require a nonblank `auth.test` team ID after
trimming whitespace. A requested workspace only checks that identity; it never
supplies a missing authenticated identity. Successful but unbound primary auth
stops Sync and Tail before archive writes or Socket Mode startup. Successful but
unbound optional user auth stops Sync and repair before writes, while concrete
optional-user authentication failures retain bot-only fallback. Doctor treats an
unbound bot identity as fatal and an unbound user identity as unavailable, and
uses the canonical user workspace ID for its DM probe.

Use workspace-scoped bot or user tokens. A valid workspace ID can accompany an
enterprise ID, but the enterprise ID alone does not identify a workspace. This
does not add organization-token resolution or a configured-workspace fallback;
the existing `users.list` request still leaves `team_id` empty.

Native request construction/execution, Retry-After parsing, HTTP status, body-read
and envelope-decode failures render only the trusted method, fixed failure phase
and optional numeric HTTP status. History/replies message-decode failures use the
same diagnostic wrapper. SDK decode errors can contain response payloads, so their
text is never appended to ordinary diagnostics, progress logs or failed-join state.
Underlying causes remain inspectable through `errors.Is`/`errors.As` and unwrapping;
the error objects themselves are not redacted. Body closure, retries, admission,
optional-auth fallback and pending-work ownership remain unchanged.

For decoded unsuccessful responses, render a native error code only when it
exactly equals `missing_scope`, `not_in_channel`, `channel_not_found`,
`invalid_auth`, `not_authed`, `account_inactive`, `token_expired`,
`token_revoked`, `is_archived` or `thread_not_found`. Other codes render
`slack <method> API response failed`; whitespace, case changes and added text do
not qualify. Explicit `ok: true` retains the SDK's error-field bypass. Channel
skip/retry decisions inspect the original code through exact machine comparisons.
Repeated channel, DM, user, history and replies cursors report only the method
and repeated-cursor condition, never the cursor value.

Underlying native codes, details and metadata remain inspectable in error causes.
This does not redact error objects, identities, progress names, successful response
metadata, successful archive data or previously stored diagnostics. This is not
a global diagnostic privacy guarantee.

### Slack export import

1. Read all four workspace JSON catalog roles and reserve every ID/name locator
   and physical payload owner before opening the archive, including excluded
   and unused fallback candidates.
2. Apply `[sync].include_dms`: omitted/true retain DM inclusion; explicit false
   vetoes every occurrence of a DM ID and requires positive non-DM evidence.
   Fully sparse records use compatible catalog privacy; any native discriminator
   disables sparse fallback. Positive native type/privacy wins over non-DM
   catalog naming; DM catalogs/native IM/MPIM always veto. Recognized native
   flag names use case-insensitive matching for both classification and veto.
3. Strict catalogs and admitted message JSON reject repeated decoded object
   keys, including nested objects and case-folded catalog ID/name/type keys.
   Reject unsupported strict formats, ambiguous locators, directory identities
   or ZIP entries/payload spans. Keep `os.Root` confinement, same-owner aliases,
   retained ZIP entries and frozen local-header metadata.
4. Strict all-excluded intake ends before archive/runtime initialization.
   Dry-run opens only an existing read-only archive, or uses absent-as-empty;
   ordinary admitted intake keeps writable initialization/migration/index repair.
5. Scan admitted bodies once before archive row writes. Validate retained raw
   channel/context identity in every recognized case spelling before projection
   or skips, choose the name/ID
   branch once, record file identity/digests, and check existing DB collisions.
   Any name-branch raw row wins; only zero rows permit ID fallback.
   Before metadata writes, check existing workspace ownership of admitted
   channels and every retained user ID. Dry-run performs the same checks.
6. Re-read each selected file through its retained source, verify its actual
   opened identity and digest, then decode that same buffer. Never rescan
   directories/catalogs or select a new fallback during execution.
7. Preserve 500-message transactions, source priority and force behavior.
   Changed/missing planned files stop intake; prior commits remain and the
   pending remainder is discarded. Report fixed DM omission counts.
8. This is future intake admission, not old-DM purging, producer authentication,
   complete capture, historical DM-origin proof, or safe-export qualification.
   Import retention and concurrent priority atomicity remain separate work.

### MCP history ownership

Persist local `mcp/history_work_v1` records keyed by workspace, channel, discovered
adapter and normalized Since. Each record contains complete presence, a raw latest
history timestamp, nullable logical pending oldest and an attempt revision.
`BeginMCPHistory` uses one immediate transaction to check existing channel ownership,
read current progress and retention, derive bounds and acquire a fresh revision.
A missing channel permits first intake; a foreign owner rejects before history HTTP.
Latest-only retains non-draft-message/retention-seed eligibility independently of
checkpoint presence. Stored message maxima never supply MCP history coverage.

Validate every returned history timestamp as finite before local filtering and
choose its numeric maximum without changing the raw key. After all admitted
history batches commit, `CompleteMCPHistory` may update only the current pending
revision. It preserves the previous latest on empty or older responses and runs
before thread traversal. Incomplete history retains its logical pending interval;
ordinary retries apply current retention even after Full. Since precedes Full and
has an isolated checkpoint. Superseded history is an explicit non-success.

Check cancellation and the history revision before and after each native or text
history tools/call, before parsing or continuing text pagination. Recheck after
materialization. Channel metadata, thread preparation, history batches (including
empty outcomes), tombstone retirement and later history-derived discovery check
that revision inside their write transaction. A newer pending or completed
revision rejects these writes and discards uncommitted materialization. The
matching completed revision remains current for discovery after history
completion, preserving the order before replies and their later failures.

This does not cancel an already dispatched call, add transport retries or undo
earlier committed batches. Workspace/user catalogs precede this per-channel
owner. Replies retain their independent thread-generation guard after selected
work is acquired. No-tool reconciliation independently validates current live
work and stored tombstones.
Whole-Sync workspace publication after completed history is not fenced by this
history-write revision.

Exclude only this exact source/type from freshness and shared progress. Merge
preserves receiver-local records; Restore clears them and cannot import foreign
coverage. First intake without a checkpoint establishes coverage from history,
subject to retention. Native request arguments and bounded-window limitations stay
unchanged. API history continues to own its separate requested horizon.

Text history still uses the configured per-loop page cap and buffers the selected
channel interval before writes. A capped scan keeps that checkpoint pending and
reports the supported recovery: temporarily raise the existing positive page
budget, retry the same channel and scope, then restore the limit after completion.
Ordinary bootstrap must not use Since, which owns a separate checkpoint. Repeated
capped scans do not accumulate pagination progress, and no opaque cursor persists
across invocations. This operator-managed recovery can increase memory and request
cost; it does not change defaults, retention authority or native window limits.

### MCP sync

1. discover the configured MCP adapter; explicit `include_dms = false` rejects
   text adapters before data calls or archive writes because they lack native type evidence
2. for strict native admission, read the complete available catalog once with
   explicit successful responses on every page; resolve direct IDs exactly,
   while omitted/true retain existing per-name requests and direct-ID shortcuts;
   under every policy, retain available observations until all selectors finish,
   then apply whole-ID alias exclusions before identity qualification
3. validate selected catalog identity before DM classification under every policy:
   a present foreign context workspace fails, including on a DM or duplicate;
   after that check, explicit false excludes shared native IM/MPIM types, while
   other unknown or conflicting types fail before writes and latest-only filtering
4. validate every retained latest-message identity across those observations
   before selected payloads proceed; explicit false with no eligible conversations
   records that outcome and leaves workspace/user/freshness state untouched
5. validate each channel page and every thread parent/reply before affected writes;
   require finite numeric history timestamps, nonblank thread timestamps, and
   replies distinct from the parent;
   check native explicit channel/context/thread fields before filtering/conversion;
   nested metadata and catalog latest-message timestamps remain optional
6. retain existing priority, retention, request defaults, and message projections
   for omitted/true; return fixed omission counts and preserve earlier successful
   history commits if a later request fails. For new native channel rows, retain
   the exact delivered channel object, including missing/duplicate fields and
   unknown identity evidence, without its catalog envelope or other objects.
   Human-text records and direct-ID stubs keep their existing archived JSON.
   Insert-only channel metadata preserves every existing row, including older
   lossy MCP records; routine sync does not repair their missing native evidence
7. require explicit `ok=true` on native history/replies under every DM policy;
   after existing decode/error/OK checks, require present arrays for native
   catalog `channels`, users `members`, and history/replies `messages` before
   projection, pagination or completion. Missing/null collections leave the page
   uncertified; explicit `[]` remains valid, including empty replies. Preserve
   the existing default-catalog/users OK policy and text adapter contract;
   retain response `has_more`/nonblank next-cursor and `is_limited` facts before
   local filtering, accumulating them across later successes and empty results
8. process valid bounded writes, but return a fixed incomplete-coverage error
   before final MCP workspace freshness if any native history/replies response reports more pages
   or a Slack history/message limit; concrete errors win and the prior complete
   freshness row remains unchanged, while metadata and valid message rows may change
9. with empty Since and a thread tool, preserve retained reply hints before history
   writes and save new page hints atomically with their message batches; drain
   selected jobs in timestamp order and retire only the matching live generation
   after complete replies; admitted revival after cancellation requeues missing
   work without replacing any extant generation, including jobs absent during this
   invocation's preparation; ordinary drain only uses its prepared/newly queued
   jobs. Incomplete history still prevents
   workspace freshness
10. with explicit Since, including Full with Since, restrict roots to identities
    themselves returned in history, using final stored ownership and reply/child
    evidence plus positive page hints; do not follow a returned child to an
    unreturned parent. Under the current pending or completed history revision,
    select and renew only those roots in one write transaction. Their shared MCP
    generations fence older ordinary or scoped replies across Since and adapters;
    unselected backlog and API jobs/skips stay untouched. Acquire all selected
    roots before replies, so a later unvisited root remains pending after failure
11. for every reply traversal, check generation and live ownership before and
    after each native or text request, each parent/reply transaction and completion;
    revoked work discards uncommitted materialization and stops pagination,
    preserving earlier commits and any newer job. Complete and empty-complete
    replies retire only the current job; errors and incomplete replies retain it.
    Since bounds root selection, not the selected thread's reply timestamps
12. during ordinary sync without a thread tool, validate selected pending work
    and reconcile authoritative stored tombstones in one transaction after valid
    history writes; do not create or renew jobs; surviving work fails actionably
    before freshness, while no-pending behavior remains unchanged

Configured workspace identity without returned context remains operator-bound,
not authenticated proof. Existing reference history pagination and MCP coverage
limits remain; no existing rows are purged and no export certification is implied.
`is_limited` denotes Slack's documented free-workspace message limit, not every
access/retention restriction. This is visible partial coverage, not pagination,
atomic sync, or resumable backfill; native tool arguments and text responses stay unchanged.

### External provider sync

1. resolve `provider:<name>` against `[[providers]]`; the provider owner rejects
   explicit `[sync].include_dms = false` before checkpoint access or process launch,
   then requires a workspace ID; omitted/true preserve provider behavior
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

Provider v1's arbitrary channel kinds and opaque raw payloads do not establish
DM exclusion. Use API sync or a supported Slack workspace JSON export for this
policy. CLI archive initialization and earlier share/config errors retain their
existing order. No request/checkpoint format changes, existing-DM purge, or
safe-export qualification are implied; `include_drafts` remains Desktop-only.

### Git share sync

Publishing rejects explicit DM exclusion before export work. Legacy table
snapshots cannot enforce that policy across archived content and derived state;
keep the archive local instead. This boundary covers publish work, not the
optional release notifier that runs before CLI dispatch.

1. reject explicit DM exclusion before snapshot acquisition/import; every importing owner entry point checks the same policy, including unchanged-manifest and historical restore paths
2. clone or open the configured share repo and read `manifest.json`
3. if the manifest generation timestamp matches, skip table import unless missing media metadata must be restored; still refresh successful-import state and restore requested media
4. otherwise merge the sharded compressed JSONL snapshot by stable identity; clear snapshot tables only for explicit restore
5. rebuild FTS rows locally
6. record last import timestamps in `sync_state`
7. copy eligible cached media when enabled, verify its hashes, and preserve destination-only media during routine merges

### Private export selection core

`internal/share` prepares and resolves a versioned, JSON-serializable private
selection plan from an existing archive file. Each operation opens the Store
read-only and selects explicit workspace, channel and message identities in one
transaction. SQLite URIs and in-memory sources are outside this file contract.
Per-record hashes bind exact scalar values, including SQL NULL versus empty
text, plus raw-payload hashes. Source changes reject resolution; derived search,
profile and unselected rows are not part of the selection.

Selected channels must have a recognized stored public kind, no stored private
flag, and retained native `is_channel=true` with explicit false private/group/IM/
MPIM flags. Reject malformed, duplicated or conflicting recognized identity/type
fields. Missing native evidence, including older MCP rows and human-text/direct-ID
records that omit these flags, cannot qualify. Newly inserted native MCP rows
retain the delivered object, but cannot supply evidence absent from that object.
Drafts and deleted messages are ineligible.
Replies require their selected eligible root in the same channel; selection
never adds parents, profiles or other rows implicitly.

The projection contains workspace/channel IDs and explicit labels (IDs by
default), then message channel ID, timestamp, user ID, chosen text, thread
timestamp and edit timestamp. Text requires an explicit keep or replace choice;
replacement may be empty. Nullable emitted scalars preserve NULL versus empty.
Raw payloads, source bindings, deletion/draft metadata and derived content remain
private. The selection core writes no files; its offline CLI owns private plan
storage and artifact commands, with no publishing path. Current native
type evidence does not establish lifetime DM origin or review quoted private
content. Operators remain responsible for selection, labels and text choices.

### Projection artifact profile

`WriteProjection` and `VerifyProjection` accept the expected in-memory projection
and an explicit lowercase 40-hex producer revision. The revision is compared
exactly, not authenticated. Inputs must have nonempty ordered unique channel and
message selections with same-channel selected-root closure; no implicit reorder.

The artifact contains exactly `manifest.json` and `messages.jsonl`. The manifest
contains format/version/revision, workspace identity and explicit label, channel
identities/labels, sorted unique nonempty referenced author IDs, and the payload
path, SHA256, byte count and row count. Message rows contain exactly the six
projection fields, preserving NULL versus empty user/thread/edit values. Both
files use the closed canonical JSON profile: one compact object plus one LF per
record, with explicit required arrays and fields. No raw data, profile expansion,
source or private selection bindings enter the artifact.

The writer exclusively creates a fresh directory with mode 0700, then fixed
messages/manifest files with mode 0600 and exclusive creation. It checks writes,
sync and close, then reopens the independent verifier before returning a receipt.
Failure leaves incomplete output for inspection and returns no receipt. It does
not overwrite, recursively clean up, or promise atomic visibility/crash durability.

Verification uses a confined opened root, exact entry inventory, distinct regular
nonsymlink files and pathname/open-handle identity checks. It hashes and parses
the same handles, checks canonical bytes, exact expected fields and referential
closure, then rechecks inventory and identity. Bounds derive from expected
content. A receipt identifies the bytes observed during verification; later
mutation invalidates that observation. This is not lifetime DM-origin proof,
content review, producer authentication, or a commit/push authorization gate.
No Git integration or schema change is part of this profile.

`CaptureProjection` uses the same verifier to return an opaque snapshot only
after all checks, checked closes and the final context check succeed. `Contents`
returns the exact accepted manifest and message bytes as immutable strings with
a value copy of the receipt; the zero snapshot is unusable. It retains no
expected-input references or artifact paths and never reopens the files. Later
file or input changes cannot alter the captured bytes. Capture memory grows with
the explicitly expected verified content; receipt-only `VerifyProjection` adds
no full encoded-message buffer. Capture grants no additional content or
publication authority and adds no CLI or Git operation.

### Offline export commands

`export prepare --db PATH --selection PATH --out PRIVATE_PLAN`,
`export build --db PATH --plan PRIVATE_PLAN --out NEW_DIR` and
`export verify --db PATH --plan PRIVATE_PLAN --dir DIR` require every path
explicitly. Dispatch precedes default config resolution and the release notifier.
Global `--config` is ignored; export-local `--config` is unsupported. No archive
initialization, automatic import, sync, Slack or Git operation occurs.

The CLI owns a private version 1 envelope with `version`, `producer_revision`
and `selection` (the core plan). Prepare/build require unique embedded Go build
settings `vcs=git`, a lowercase 40-hex `vcs.revision` and `vcs.modified=false`.
Build requires the current revision to match the envelope. Verify uses the plan's
revision, permitting newer verifiers without the old producing binary. Both
build and verify freshly resolve bindings; artifact data never defines expected
content. Missing or dirty build metadata fails with clean-build guidance.

Selection input allows formatting whitespace but otherwise must match the
closed typed JSON field order, spelling and explicit fields. Plans require exact
compact canonical JSON plus one LF. Roundtrip equality rejects duplicate,
unknown, case-varied, escaped or missing keys. Core selection semantics still
own required arrays and scalar validation. Selection and plan files must be regular nonsymlink
files with checked read/close and pathname/handle identity. Prepare exclusively
creates a 0600 plan in an existing parent and checks write/sync/close; failure
retains partial output. Commands emit counts or five tagged receipt fields only,
never plan contents or private replacement text. Verification receipts remain
observations, not content approval or publication authority.

### Desktop-local sync

1. discover the Slack Desktop path
2. snapshot/copy source artifacts before parsing
3. parse `storage/root-state.json`
4. inspect IndexedDB and Local Storage artifacts
5. apply `include_drafts = false`, then prepare all admitted conversation records before desktop summaries or writes:
   - explicit `include_dms = false` requires selected native public/private channel metadata; any decoded IM/MPIM, unknown, or conflicting observation for that workspace/channel vetoes admission
   - retain richest-state selection and legacy payload/duplicate behavior for omitted/true; preserve decoder identity evidence separately from persisted raw payloads
   - keep attachments in their parent message payload; do not discover them as separate messages
   - freeze workspace/name candidates before filtering and carry each resolved owner into persistence
   - reject otherwise retained channel/message container identity conflicts under every policy before any desktop writes
   - under explicit `include_dms = false`, require every draft destination to pass type, workspace, and selectors and resolve to one workspace; otherwise omit the whole draft
   - join recent/read-marker records to admitted conversations; omit unattributed download/expandable counts under explicit DM exclusion
   - report admitted counts and bounded omission reasons, including unavailable/partial decoding; keep raw Inspect/Doctor diagnostics and independent profile/status handling
6. ingest supported desktop-local metadata:
   - workspace/user metadata from `localConfig_v2`
   - cached channel metadata, member profiles, and channel message history from IndexedDB redux persistence blobs when `node` is available
   - cached thread roots and cached reply messages from IndexedDB redux persistence blobs when present
   - draft bodies and thread draft destinations, unless `include_drafts = false`
   - recent-channel hints
   - `conversations.mark` read markers
   - custom-status state
   - IndexedDB object store inventory for drift detection

Sent Redux message writes enforce the strongest current global, workspace, or
channel retention floor inside each batch transaction, including the final
partial batch. A reply uses its parent timestamp; a message at the cutoff is
eligible. An exact existing row below the floor remains eligible for updates
under source-priority rules. Preparing cache data before a purge does not freeze
the retention decision. This shared write path covers desktop/wiretap, watch,
and all/hybrid desktop ingestion. Metadata, inventory and checkpoints may
refresh even when sent messages are omitted; admission counts describe prepared
input, not inserted rows. Draft retention remains a separate boundary.

Desktop read markers use source `desktop`, entity type `read_marker_v1`,
compact JSON `[resolved_workspace_id,channel_id]` keys and unchanged scalar
timestamp values. The writer uses the owner retained by admission, not the
persisted call's workspace. Users/calls within a tuple keep their existing
overwrite order; unsorted calls have no prescribed winner or maximum-timestamp
rule. Legacy channel-only `read_marker` rows are never read, migrated or
dual-written by intake. Generic snapshot export/merge/Restore and message-purge
behavior remain unchanged, as do marker counts and freshness accounting.
Channel-hint projection and per-user read-state semantics are separate.

## Go Package Layout

```text
cmd/slacrawl/
internal/cli/
internal/config/
internal/importer/
internal/mcpclient/
internal/media/
internal/provider/
internal/report/
internal/share/
internal/slackapi/
internal/slackdesktop/
internal/slackmcp/
internal/store/
internal/store/sqlc/      # query and schema inputs
internal/store/storedb/   # generated sqlc wrappers
internal/search/
internal/syncer/
```
