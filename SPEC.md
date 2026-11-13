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
- Go `1.27.0+` is installed; the preferred build toolchain is `1.27.2`
- desktop-local Slack data may exist under:
  - `~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack`
  - `${XDG_CONFIG_HOME}/Slack`
  - `~/.config/Slack`

## Slack Data Model Notes

Important Slack facts that drive the schema:

- messages are scoped by `(channel_id, ts)`
- threads remain message relationships via `thread_ts`
- API sync prefers a user token for channel replies; without one configured, the authenticated bot can fetch replies within its scopes and membership
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
- `native_changes`
- `native_change_state`

`channels.kind` values include:

- `public_channel`
- `private_channel`
- `public`
- `private`
- `im`
- `mpim`

Optional later:

- `message_embeddings`

### Native change journal

Schema 9 journals committed inserts, updates and hard deletes for `workspaces`,
`channels`, `users`, `messages`, `message_files`, `message_mentions` and
`sync_state`. SQLite triggers capture direct writes and acquisition checkpoints
in the same transaction as the native rows. A rejected write or rollback leaves
no change entry. NULL-safe updates emit only when a stored value changes;
`updated_at` changes count. Key or explicit workspace changes emit an old-key
removal followed by the new row observation. Threads remain message rows.

`native_changes` contains `sequence` (SQLite AUTOINCREMENT), random `entry_id`,
`table_name`, `operation` (`i`, `u`, `d`), typed `key_json`, optional
`workspace_id` and `recorded_at`. Keys preserve native strings and SQL NULL.
There are no row bodies, raw payloads or checkpoint values. Mention workspace
context comes from its retained parent or channel and can be NULL. Sync-state
scope must be interpreted from its native composite key; no workspace is guessed.
The journal is private local metadata, not permission to export any record.
A tombstone is a row update; a hard delete is an explicit removal. Neither proves
an upstream Slack deletion. `message_events` remains separate retained history;
FTS, event heads and embedding jobs are derived and are not journaled.

The singleton `native_change_state` binds contract version 1, random generation,
floor sequence and floor entry anchor. `status --json` and legacy status JSON
include additive `native_changes` metadata with that floor and the current head
sequence/entry anchor. An empty generation uses the floor anchor as its head.
Version 8 and older read-only archives omit this metadata until writable upgrade.
Status validates journal structures in its read snapshot and reports repair
errors instead of certifying a broken journal. Writable open repairs missing or
changed journal structures transactionally and rotates the generation. Valid old
entries remain below the new floor; malformed journal tables alone are rebuilt.
Native archive rows and acquisition state are not replaced by journal repair.

Upgrade establishes a baseline, not invented historical events. Exact share
restore rotates the generation and floor in the restore transaction; ordinary
merge produces native changes without replacing the generation. The journal and
state are excluded from share export and rejected as incoming snapshot tables.
Generation changes require a future consumer to reconcile a consistent local
snapshot. Sequence plus entry anchor distinguishes divergent copies; sequence
alone is not a sufficient delivery cursor.

This foundation does not implement capture, delivery cursors, an outbox, HTTP
workers or compaction. No timer or row deletion runs. Seven-day retention is a
proposal only; activation needs a measured storage budget and an explicit gap
reconciliation contract. Journal writes add SQLite/WAL storage proportional to
actual native changes, including routine acquisition timestamp updates. It does
not establish acquisition completeness, current freshness or successful delivery.

### Bounded local native capture

`store.CaptureNativeChanges` opens an existing absolute regular archive file
read-only and owns one SQLite read transaction. It returns fully materialized
local observations and closes the transaction before returning. It writes no
acquisition or delivery cursor. Only Darwin/Linux device-and-inode identity is
currently supported; other platforms, symlinks and identity mismatches fail.

The archive owner must keep the pathname bound to one physical file throughout
capture. Concurrent SQLite writes are supported; renaming or replacing the
database requires stopping capture first. Before/after pathname checks detect
persistent replacement but do not bind SQLite's opened handle against a transient
swap away and back. That unsupported local-owner race is an accepted limitation;
these checks are not a security boundary against a process controlling the path.

The caller supplies an explicitly authorized single-workspace/channel scope and
a qualified baseline containing file identity, scope SHA256 and the journal's
generation/sequence/entry anchor. The API does not create that baseline or verify
an external delivery receipt. Hashes and anchors prove consistency, not authority.
Workspace metadata and selected channels/messages/files/mentions are included.
Workspace users require explicit opt-in; sync-state rows require exact registered
source/entity-type/entity-ID keys. Acquisition DM defaults grant nothing here.

`NativeChannelAdmissionSHA256` hashes version 1 plus stored `id`, `workspace_id`,
`kind`, `is_private`, `is_shared` and these raw channel facts:

- required `id` and all six explicit booleans: `is_channel`, `is_group`, `is_im`,
  `is_mpim`, `is_private`, `is_shared`;
- optional `channel_id`, `workspace_id`, `team_id`, `context_team_id`, preserving
  field absence versus a present empty value. Nonempty identities must match the
  registered channel/workspace; a channel identity cannot be empty.

Stored kind/privacy/shared facts must agree with the native flags. Unknown kinds,
missing/null/uninterpretable flags, contradictory types, foreign identities,
invalid UTF-8 and duplicate raw object keys (case-insensitive) fail. Old or sparse
Desktop rows need explicit local reconciliation; capture does not guess missing
facts. Name/topic/timestamp and other raw payload changes do not alone invalidate
admission. `NativeCaptureScopeSHA256` binds the sorted unique channel admissions,
workspace, user opt-in and sorted exact sync-state keys. A changed or missing
registered channel requires scope reconciliation rather than implicit expansion
or inferred removal of its contents.

Capture validates required journal structures, generation, floor/head and the
exact saved entry anchor in its read snapshot. It reads the entire `(saved,H]`
interval or fails, including gaps among unselected events, never returning partial
progress. Positive `MaxEvents`,
`MaxBytes` and a context deadline are required. The byte budget counts admission
facts, scanned journal fields and materialized native column names/scalars; it is
a logical input/result budget, not an RSS or wire-size promise. Text is counted
as UTF-8 bytes, integer scalars as nine bytes and NULL as one byte. SQLite length
queries reject oversized bodies before loading them. Table read plans are reused
within a capture only. No compaction or network operation occurs.

Repeated keys coalesce into their current row at `H`. A hard delete or old-scope
move supplies an explicit removal observation; delete/reinsert in the same scope
returns the current row. Foreign ownership is filtered before materializing row
bodies. Missing rows without matching removal evidence fail. Native string IDs,
NULL versus empty, integer precision, timestamp spelling and valid UTF-8 including
NUL survive capture; unexpected storage classes or invalid UTF-8 fail.

`NativeCapture` is **not a service payload**. Native raw JSON and media/cache paths
remain local captured data. File identities and archive paths also stay local.
An egress owner must explicitly select/translate allowed columns before
serialization. Capture establishes neither historical completeness nor successful
delivery. The private projection and local outbox below supply delivery data and
durability contracts; binding activation and the receiver remain separate work.

### Bounded native seed capture

`store.CaptureNativeSeed` reads the current six content tables at one real native
journal head H. It requires an explicitly selected physical file, the existing
single-workspace/channel admission scope and its checksum, positive `MaxRows`
and `MaxBytes`, and a context deadline. It uses one read-only transaction for
schema 9/journal validation, channel admission, head selection and all table
reads. The stable-path ownership precondition above also applies. Concurrent
SQLite writers can continue; their later changes are outside this snapshot.

The fully materialized `NativeSeed` returns local file identity, scope checksum,
H's exact generation/sequence/entry anchor, and six ordered tables with native
columns and typed rows. H may be zero after upgrading a populated pre-journal
archive. Capturing a seed neither appends a synthetic journal entry nor invents
a `From` cursor. It writes no source data, schema, acquisition or delivery state.

The tables are `workspaces`, `channels`, `users`, `messages`, `message_files`
and `message_mentions`, in that order. Workspace/channel selection and explicit
workspace-user opt-in match native incremental capture. Mentions with a foreign
message parent are excluded; a missing parent can use the admitted channel.
Users without opt-in and every other empty table retain their native columns.
Rows use binary native-key ordering with a local rowid tie-breaker, preserving
multiple nullable-key rows without exporting rowid. Retained deletion fields
remain observations; absence does not invent a removal or historical evidence.

Before loading row bodies, the reader checks scoped counts and scalar sizes
across all six tables. Loading also checks actual row counts and byte usage.
The byte bound covers native scalar payloads, column-name bytes once per table
and the existing channel-admission byte accounting: NULL costs one byte,
integers nine, text one plus its UTF-8 byte length. It is a logical data bound,
not a process RSS or encoded wire-size guarantee. Invalid UTF-8, unexpected
SQLite storage classes, changed admission, limits and cancellation return no
partial seed. Native NULL, empty text, exact int64 values, NUL, raw JSON and
timestamp spelling follow the same decoder as incremental capture.

`remotedelivery.ProjectPrivateSeed` applies the private profile below, requiring
all six tables in order and validating each native layout even when empty. It
returns upserts only and shares the existing column/scalar projection owner.
File identity, admission facts and the native anchor are not row payload fields.
Neither API serializes a seed manifest, stages durable chunks, initializes an
outbox or sends data. A later verified service completion receipt must qualify
the seed and H before they can become an incremental delivery baseline. A seed
does not prove acquisition coverage, freshness or successful central import.

### Private archive service projection

`remotedelivery.ProjectPrivateArchive` is a pure translation from an already
admitted `NativeCapture` to Crawlkit `remote.Change` values. Its fixed profile is
`slacrawl-private-archive-v1`. It does not open SQLite, authorize a binding,
advance a cursor, send HTTP or create an outbox. Native file identity, scope
admission metadata and archive paths are not projected.

This profile retains private source evidence. It is not a report or public
projection: those consumers need their own visibility and sanitization owners.
`raw_json` remains opaque text, including original whitespace and unmodeled
content; it is neither re-encoded nor regex-redacted. Before activation, the
binding owner must qualify the captured payload provenance and authorize this
private profile. Unknown raw provenance does not become safe through projection.

The key and row orders are fixed independently of native schema column order:

| Table | Key columns | Row columns, in order |
| --- | --- | --- |
| `workspaces` | `id` | `id name domain enterprise_id raw_json updated_at` |
| `channels` | `id` | `id workspace_id name kind topic purpose is_private is_archived is_shared is_general raw_json updated_at` |
| `users` | `id` | `id workspace_id name real_name display_name title is_bot is_deleted raw_json updated_at` |
| `messages` | `channel_id ts` | `channel_id ts workspace_id user_id subtype client_msg_id thread_ts parent_user_id text normalized_text reply_count latest_reply edited_ts deleted_ts source_rank source_name raw_json updated_at` |
| `message_files` | `channel_id ts file_id` | `workspace_id channel_id ts file_id user_id name title mimetype filetype pretty_type mode size url_private url_private_download permalink is_public plain_text preview_plain_text content_sha256 content_size fetched_at raw_json updated_at deleted_at deletion_source deletion_reason` |
| `message_mentions` | `channel_id ts mention_type target_id` | `channel_id ts mention_type target_id display_text deleted_at deletion_source deletion_reason updated_at` |

All keys remain text or native NULL. Integer columns are the channel flags
`is_private is_archived is_shared is_general`, user flags `is_bot is_deleted`,
message `reply_count source_rank`, and file `size is_public content_size`.
Other columns are text; native NULL remains distinct from empty text. Integer
values use exact signed 64-bit decimal strings. UTF-8 including NUL, timestamp
spellings and raw text are preserved. Unknown tables, missing/extra/duplicate
content columns, key/row disagreement or incompatible scalar types fail without
returning partial changes.

`message_files.media_path`, `fetch_status` and `fetch_error` are the only excluded
content columns. Retained hashes, sizes and `fetched_at` are source observations;
they do not prove that the service possesses any attachment bytes. These named
exclusions do not strip similarly named content from opaque raw payloads.

All `sync_state` rows and removal observations are excluded. Acquisition
checkpoints remain collector-owned; this profile makes no coverage claim. A
sync-only capture produces an empty, non-null changes array. Its complete native
watermark still requires the delivery owner's normal receipt/acknowledgment
flow, rather than being silently skipped or treated as already delivered.

Upserts preserve capture row order, followed by removals in capture removal
order. Removals contain only the table and native key; local removal provenance
does not enter the payload. Capture already owns coalescing, so a delete/reinsert
returns the current upsert while a key move retains its old-key removal.
Projection does not split batches or enforce wire limits; the local outbox owns
partitioning and Crawlkit enforces its change-batch limits before encoding.

### Local delivery outbox

`remotedelivery.InitOutbox` exclusively creates a separate private SQLite file
from a caller-supplied qualified `Registration`. It does not inspect a native
archive or infer a baseline. Registration fixes the service origin, app/archive,
private profile, native file identity, scope SHA256 and initial native/service
cursors. Origin validation accepts HTTP for isolated previews; transport and
authentication policy remain with the Crawlkit client. Credentials and source
paths are not stored. Registration cannot be changed or reset through this API.

Only literal absolute regular file paths are supported, not SQLite URIs,
`:memory:` or symlinks. The owner must keep each outbox pathname bound to its
physical file while open. `OpenOutbox` first recognizes the schema and registration
read-only, before a writable helper can change permissions or journal mode.
It rejects unknown/native archives instead of initializing or migrating them.
A recognized version-1 delivery outbox upgrades to version 2 on open, in one
`FULL`/`BEGIN IMMEDIATE` transaction. The constrained tables are rebuilt and
existing body/receipt BLOBs, hashes, IDs, completed flags and cursor values are
copied without JSON decoding or re-encoding. A failed migration rolls back the
schema and data; concurrent openers recheck the version under the write lock.
Version-1 binaries reject version-2 outboxes. Stop older outbox users before
upgrading; there is no downgrade or mixed-version write-through contract.
Initialization creates a mode-0600 database and mode-0700 missing parent
directories; a failed initialization retains its file for diagnosis.

The three tables are `binding`, `captures` and `parts`. Version 2 records an
explicit `baseline_state`; existing registrations migrate as `qualified`.
Seed registrations start `unqualified` with NULL native anchors. Capture kind
and the same receipt ledger distinguish seed requests from later deltas.
Delta limits remain 64 parts and 500 changes per part; wider seed storage
constraints do not expand that delta delivery contract.

A unique partial index permits one pending capture. `Stage` calls the private projector and partitions
its materialized result outside the write transaction. It then uses
`BEGIN IMMEDIATE` to recheck the saved baseline and pending state, and atomically
persist the complete ordered capture: its randomly allocated ID, every exact
request body, SHA256 and final native anchor. `synchronous=FULL` is set on each
checked-out write connection before the transaction. A concurrent stage loses
without replacing pending data; an uncertain commit is resolved by reopening
and reading `State`/`NextPart`, never by silently recapturing source rows.

Native wire anchors are
`slacrawl-native-v1:<generation32hex>:<nonnegative canonical int64>:<entry32hex>`.
File identity and scope admission facts stay in the local registration. All
parts have the same native range. The first expected service cursor is the last
completed capture's cursor; subsequent parts name the preceding batch ID.
Bounds are 4 MiB encoded per part, 500 changes per part and 64 parts, plus a
required positive caller `MaxEncodedBytes` no greater than 256 MiB. These are
logical payload bounds, not RSS promises. An oversized single change or complete
capture fails without partial staging. Zero native progress returns
`ErrNoNativeProgress`; positive progress containing only excluded observations
persists one empty changes part.

`NextPart` returns the first unreceipted part's persisted bytes and verifies its
hash. Its transaction is closed before return. Multiple dispatchers may obtain
the same bytes. `RecordReceipt` matches the stored part's route, binding, batch
ID, hash, cursor and positive canonical int64 service sequence. Receipts must
arrive in part order. Recording the final receipt, completing the capture and
advancing both completed cursors share one transaction. The native archive is
never written. Exact duplicate receipts are harmless, including after a newer
capture is pending; a changed `Replayed` flag does not replace the first receipt.

After a process exit, staged bytes and intermediate receipts resume unchanged,
even if the source has since changed. A failed final checkpoint write also rolls
back its receipt. Completed captures, bodies and receipts remain retained;
pruning, retention activation and reconciliation/reset APIs are separate work.
The local outbox API does not activate a scheduler or live registration.

### Durable private seed staging

`InitSeedOutbox` exclusively creates an unqualified outbox from explicit
`SeedRegistration`: service origin/app/archive/binding/profile, physical native
file identity and `NativeCaptureScope`. It computes the existing source-owned
scope checksum and stores the derived workspace/channel/user wire scope. There
is no native cursor until completion. Registration hashes bind consistency;
they do not grant publication authority or qualify raw-payload provenance.

`Outbox.StageSeed` accepts a fully materialized `NativeSeed`, requires matching
file/scope identity, and calls `ProjectPrivateSeed` itself. It uses the captured
real `Through`, including H0, without writing the source or inventing a change.
A random seed ID fixes the request identities. Six tables remain in profile
order, including empty tables. Chunks contain only one table, up to 500 rows and
4 MiB each, with at most 8192 chunks total. The source preserves exact typed
values and every row, including ambiguous natural keys.

Chunks are encoded first, followed by the manifest and completion request.
Each descriptor hashes its exact chunk bytes; a table hash covers the ordered
binary chunk digests. Completion names the exact manifest hash. The same three
SQLite tables atomically retain the manifest, all chunks and completion before
any HTTP request. Local part zero is manifest, parts 1..N are chunks 0..N-1,
and part N+1 is completion. No parallel seed checkpoint or acknowledgment owner
exists. A positive caller `MaxEncodedBytes` bounds all encoded requests together
and cannot exceed `(8192+2)*4 MiB`. These materialized payload limits do not bound
process RSS; callers must also bound native capture. Oversize rows or complete
seeds fail without partial staging. Existing pending work cannot be replaced.

A NULL key component or duplicate natural-key vector records the immutable
`ambiguous_keys` hold. Begin/chunks may retain that evidence remotely, but
`NextPart` refuses the final request and `RecordReceipt` refuses completion even
if supplied directly. The baseline remains unqualified. Invalid native scalar
classes or UTF-8 fail projection; no row is silently removed or assigned an
invented identity. This preflight does not replace independent receiver
eligibility validation or solve ambiguous keys introduced by later changes.

Only the matching final completion receipt, after all ordered receipts, changes
`baseline_state` to `qualified` and records H and the final service cursor as both
initial and saved baseline in one transaction. Intermediate receipts cannot
qualify it. A failed final write also rolls back that receipt; retry sends the
same saved bytes and receives the original service receipt. A qualified seed can
then use ordinary native capture and delta staging. Seed receipts remain valid
replays after subsequent deltas. Counts and completion do not establish upstream
freshness or acquisition completeness.

A held attempt has no flag-clearing, reset or replacement API. Retain the outbox
and receiver evidence, reconcile the native identity ambiguity through its owner,
then explicitly qualify a new source/binding/outbox. The outbox does not create a
receiver registration, listener, collector or scheduler.

### Operator delivery commands

`delivery init`, `delivery stage-seed`, `delivery stage-changes` and
`delivery deliver` route before
personal config discovery, release notifications and automatic archive opening.
They reject `--config`, including an explicitly empty value. There is no default
workspace, channel enumeration, source synchronization, schema migration,
receiver registration or scheduler. Native paths must name an existing literal
absolute regular file; source ownership must satisfy capture's stable-path rule.

- `init --db PATH --scope PATH --outbox NEW_PATH --origin URL --archive ID
  --binding ID` pins physical file identity and the explicit scope in a new
  unqualified outbox. It does not open native SQLite or invent a journal head.
- `stage-seed --db PATH --scope PATH --outbox PATH --max-rows N
  --max-native-bytes N --max-encoded-bytes N --timeout D` requires positive
  limits and a deadline, matching registered file/scope and no pending capture.
  It materializes one read-only native seed, closes its transaction, then stages
  all exact requests. The byte limits bound logical scalar/metadata and encoded
  payloads, not process RSS. Encoded bytes cannot exceed `(8192+2)*4 MiB`.
- `stage-changes --db PATH --scope PATH --outbox PATH --max-events N
  --max-native-bytes N --max-encoded-bytes N --timeout D` requires a qualified
  baseline and no pending capture. It uses the latest completed outbox baseline,
  not the initial seed anchor. All bounds/deadline are positive; encoded bytes
  cannot exceed `64*4 MiB`. `max-events` bounds the complete `(saved,H]` native
  journal interval, including excluded events, not the projected row count.
  Capture closes its native read transaction before the existing `Stage` owner
  projects and atomically persists all parts. Staging makes no HTTP/credential
  call and leaves the completed native/service cursors unchanged.
- `deliver --outbox PATH --max-parts N --timeout D [--token-env NAME]` resumes
  persisted requests against the stored endpoint, without reading the native
  archive or scope file. The part bound is 1..64 and the deadline is positive.
  The token comes only from Crawlkit's `EnvTokenProvider`, default environment
  variable `CRAWL_REMOTE_TOKEN`; no Slack/MCP token fallback, argument containing
  the token value, credential persistence or new config section exists.

Existing outboxes use `OpenCurrentOutbox`: older schemas are refused during
read-only recognition, before writable opening. The library's existing
`OpenOutbox` upgrade contract is unchanged; upgrades require a separate explicit
operation outside these commands. No migration command is included.

The private scope JSON is at most 1 MiB, from a regular nonsymlink file. Following
export selection conventions, all fields must appear in the order and JSON
spelling below (whitespace is allowed). Unknown, duplicate or missing fields,
NULL arrays, invalid UTF-8 and duplicate channel/key identities are refused.

```json
{"version":1,"workspace_id":"T_EXAMPLE","channels":[{"channel_id":"C_EXAMPLE","admission_sha256":"<64 lowercase hex>"}],"include_workspace_users":false,"sync_state_keys":[]}
```

Each optional admitted sync-state key uses exactly `source_name`, `entity_type`,
`entity_id`, in that order. There is no inferred key family. The source-owned
`NativeChannelAdmissionSHA256` computes the channel digest from previously
reviewed facts; this CLI does not prepare or authorize scope. The scope document
and raw-payload provenance require separate operator qualification. All
`sync_state` remains excluded from the private wire profile.

JSON/text/log output contains only operation status, baseline state, capture ID,
counts, a hold if present and the staged `native_anchor`: `generation_id`, decimal
string `sequence`, and `entry_id`. These are the real H components for separate
receiver setup, spelling the wire anchor as
`slacrawl-native-v1:<generation_id>:<sequence>:<entry_id>`. Output omits source
rows, paths, full admission facts and credentials. Initialization/staging is not
baseline qualification. Only the recorded final receipt qualifies it; even that
does not establish source freshness or complete acquisition.

A failed delivery reports bounded progress and an error while retaining exact
pending bytes. A held seed reports `ambiguous_keys` and fails without clearing
its hold. After an uncertain stage or output failure, inspect the retained
outbox through its owner before retrying; never delete/recreate it to retry.
`stage-changes` returns successful `status=no_native_progress` at unchanged H,
without creating a capture, part or receipt. When H advances but the admitted
private projection is empty, it stages one empty changes part; its matching
receipt must still advance the baseline. Incremental output reports native
`events_read`, materialized `rows` and `removals` counts when nonzero, not remote
effect counts. A staged anchor is not the latest completed baseline until receipt.

Changed generation/file/scope/admission, a cursor below the retained floor, an
incorrect or missing entry anchor, interval gaps, unsupported schema or
uninterpretable data cause reconciliation refusal. An over-budget interval fails
whole; the CLI never clamps the cursor, invents progress, retries with larger
bounds or automatically reseeds. Existing retained journal data can be delivered
consistently even when acquisition is old; no progress does not prove freshness.
No force/reset/rebind or upstream acquisition command is provided.

### Explicit pending delivery

`Outbox.DeliverPending` connects an already registered outbox to the unchanged
Crawlkit transport. It constructs that client from the registration's origin and
uses its app/archive; callers supply only trusted in-memory HTTP and token
adapters. There is no caller-selected endpoint and no stored credential. Crawlkit
continues to own authenticated transport and redirect policy, including HTTPS
and the loopback HTTP exception. The explicit operator CLI below invokes this library; no configuration default,
collector, scheduler or listener invokes it.

Each invocation requires `MaxParts` from 1 to 64 and a positive `MaxDuration`.
The duration creates a child context and preserves an earlier parent deadline.
Caller-owned HTTP transports and token providers must honor that context; the
driver creates no background work. It pins the capture pending at entry, reads
its exact persisted bytes through `NextPart`, sends each selected part once, and
durably accepts its matching receipt through `RecordReceipt` before proceeding.
It neither captures nor stages native data, and holds no SQLite transaction over
HTTP. A concurrently staged successor is left for a later invocation. For seed
captures it decodes the saved manifest once per invocation and reuses indexed
descriptors while dispatching begin/chunk/completion through the shared client.
The same 64-request invocation budget applies to larger, resumable seeds.

`DeliveryResult.Status` distinguishes `idle` (no capture pending at entry),
`complete` (the pinned capture completed), `part_limit` (the part budget ended
with that capture still pending), and `pending` on a failure after identifying
the capture. Errors before identification can return an empty result. The result
counts successful receipt-recording calls, including an exact receipt already
recorded by another dispatcher; it does not count unique network effects. The
completed status is about that pinned capture, not a claim that the outbox is
still empty or the native archive is current.

Network, response-validation and local receipt failures stop the invocation
immediately. There are no implicit retries or replacement captures. A lost reply
or uncertain local commit is reconciled from the retained outbox; a later explicit
call resumes the same persisted part. Failed calls can therefore undercount
effects whose acknowledgment was lost. Printable errors expose only a fixed
operation classification and, when applicable, `context.Canceled` or
`context.DeadlineExceeded` identity. A held seed also preserves the safe
`ErrSeedBaselineHeld` identity. Driver errors do not wrap private remote or
SQLite diagnostics, URLs or tokens. Receiver-side source/binding registration,
credential-selection CLI policy and scheduled activation remain separate work.

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
11. backfill thread replies with the user token when it authenticates to the selected workspace; reuse primary user authentication without another auth call
    - when no user token is configured, API Sync uses its authenticated bot for channel replies with `api-bot` rank 2 provenance; user replies retain `api-user` rank 1
    - never switch reply credentials after configured user authentication or reply access fails; bot scope/membership errors retain pending work
    - keep workspace full-thread eligibility gated on valid user auth; bot-only success remains partial, and Tail repair keeps its user-token reply path
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
auth still permits bot history with partial reply coverage, but disables replies
rather than changing credentials. With no user configured, API Sync can fetch
bot-accessible channel replies while keeping workspace coverage partial. Doctor
treats failed bot auth as fatal and failed user auth as unavailable; Tail authenticates its bot
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
