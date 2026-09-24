# Changelog

## Unreleased

- Fix desktop sync rejecting timestamped attachments and quoted messages as conflicting standalone messages; retain attachments in their parent payload under every DM policy. Thanks @ViaxCo for the fix and @vitorsj for the report! (#254, #255)
- Clarify desktop upgrade recovery: the schema-8 migration introduced in v0.9.0 happens on writable open even when sync fails; downgrading requires a consistent pre-upgrade backup.

## 0.10.0 - 2026-09-21

**Highlights:** Enforce configured DM and draft exclusions, preserve incomplete API/MCP sync work, and add offline export preparation and verification. Byte verification does not authorize publication.

- Add offline `export prepare`, `export build` and `export verify` commands with explicit archive and private selection/plan paths. Bind preparation/building to a clean embedded Git revision, recheck current source bindings before building or verifying, and emit only counts or observed-byte receipts. Keep private plans separate from the two-file artifact; no config loading, automatic imports, release checks, Slack or Git publishing runs on this path.

- Enforce `sync.include_dms = false` before API sync persists conversation metadata or messages; reject unknown conversation types under this policy and mismatched channel identities under every policy. Previously archived rows are unchanged.

- Allow desktop/wiretap sync, watch, and all/hybrid sync to exclude unsent drafts with `[slack.desktop].include_drafts = false`; preserve the default and leave already archived drafts unchanged.

- Support user-only API sync across discovery, profiles, history and source-specific completion state; preserve configured-bot precedence, user-token replies and bot/app Tail requirements. Doctor now authenticates user-only credentials, keeps global coverage separate from named-workspace aggregation, and reports one ordered list of bot/user channel skips without changing stored status. Thanks @vincentkoc! (#215, #218)

- Track MCP channel-history completion separately from stored messages and replies. Keep failed or incomplete intervals pending, preserve completed empty scans, and retry with current retention bounds. Prevent newer replies or API/Desktop rows from skipping unread history; reject non-finite history timestamps before filtering. Keep checkpoints local to the archive and out of freshness timestamps. First intake after upgrade, fresh import or whole-snapshot restore establishes its own history checkpoint; native server window limits remain unchanged.

- Update CrawlKit to 0.16.3, SQLite to 1.59.0, and terminal support dependencies while retaining the Go 1.27.0 minimum and SQLite's required libc 1.75.7.

- Add internal capture of exact independently verified projection bytes, so later consumers can use immutable manifest/message contents without reopening changed files. Return no usable snapshot on failure; retain the receipt-only verifier path and existing content/publication limits.

- Preserve exact native MCP channel objects when inserting new archive channels, so later selection can inspect the delivered type and identity evidence. Keep missing and conflicting fields intact without retaining the whole catalog. Existing channels, human-text records and direct-ID stubs stay unchanged; routine sync does not repair older lossy metadata or certify DM origin/content safety.

- Add an internal two-file projection artifact writer and independent verifier. Exclusively create a fresh destination, preserve explicit message fields and nullable values, and verify canonical JSON, expected content, checksums and file identities before returning a receipt. Failed output remains for inspection; this adds no CLI/Git integration or publication approval and makes no atomic visibility or crash-durability claim.

- Add an internal read-only selection core for future export projections. Bind explicit workspace/channel/message rows, require retained native public-channel evidence and selected thread parents, and resolve only explicit labels and message fields with keep/replace text choices. Private plans are serializable; no CLI, output writer or publication path is added, and current type evidence does not certify historical origin or content safety.

- Reject legacy Git snapshot publishing when `sync.include_dms = false`, before archive initialization, cache locking, Git operations or output writes. Keep the archive local; `--no-commit` and `--no-media` do not bypass the gate. Omitted/true retain unfiltered private snapshot publishing, including already archived DMs and drafts.

- Verify and document importing Slackdump-converted ZIP and directory exports from database and chunk archives. Preserve real synthetic converter fixtures for thread identity, DM exclusion, source/FTS consistency and repeat-import coverage; no importer behavior or upstream dependency changes.

- Explain recovery when text MCP history exceeds the page limit: use a temporary larger positive budget for the same channel and scope, then restore the normal limit after completion. Large first scans require this operator-managed bootstrap; repeated capped attempts do not resume across invocations.

- Preserve unvisited API thread-skip diagnostics during restricted or incomplete Full syncs, and explain retained thread work in Doctor's partial-coverage output. Require unrestricted traversal before bulk cleanup, retain generation guards, and keep observed omissions visible despite later success. Thanks @vincentkoc! (#229, #230)

- Respect purge retention floors when replaying sent Desktop cache messages, including replies to expired roots and caches prepared before purge. Preserve existing-row enrichment and source priority. Store read markers by workspace and channel without guessing ownership for legacy markers. Thanks @vincentkoc! (#245, #246)

- Fence MCP history and scoped reply writes against superseded work, and require native response arrays before accepting pages. Preserve newer content, earlier committed pages and durable retry work without consuming unselected backlog. Thanks @vincentkoc! (#242, #243, #244)

- Fence API history and reply writes to the current scan, retain collision intervals for retry, and publish coverage atomically with pending-work checks. Preserve newer writes and checkpoints when requests overlap, and keep Status and Doctor truthful while work remains. Thanks @vincentkoc! (#239, #240, #241)

- Keep native API payloads, arbitrary error strings, and pagination cursors out of diagnostics while preserving typed causes and known machine errors. Report failed DM capability probes and caller cancellation truthfully in Doctor. Thanks @vincentkoc! (#236, #237, #238)

- Require explicit native API success, present page collections, and a bound authenticated workspace before accepting data or completing sync work. Preserve earlier valid writes, pending retries, credential precedence, and typed Slack errors. Thanks @vincentkoc! (#231, #232, #233, #234, #235)

- Preserve unfinished MCP reply work across history-hint loss, failed batches, and restarts; guard ordinary replies by generation and reconcile committed tombstones. Keep explicit `--since` and `--full --since` restricted to returned roots without consuming older backlog. Thanks @vincentkoc! (#217, #220)

- Persist retained API thread work across lost reply hints, failures, restarts, and token changes; isolate unavailable roots so healthy threads and later channels continue. Keep pending work generation-guarded, local to the archive, excluded from freshness, and removed atomically by deletion or purge. Thanks @vincentkoc! (#217, #219)

- Reject external provider v1 sync when `sync.include_dms = false`, before checkpoint access or adapter launch. Use API sync or a supported Slack workspace JSON export for DM exclusion. Omitted/true retain provider requests and scoped cursors; existing archive rows and CLI initialization remain unchanged.

- Keep API history intervals pending when a terminal history/replies page reports more results without a continuation cursor, or any accessible history page reports a history/message limit. Preserve valid writes and previous successful coverage, follow available cursors, and retry the same pending interval after correction. Periodic repair shares these checks; capability probes remain usable.

- Honor explicit `sync.include_dms = false` for Slack workspace JSON export imports before archive initialization. Reserve all conversation locators, omit DM bodies, and reject unqualified types, duplicate JSON keys, or identity conflicts. Check existing channel/user ownership before metadata writes under every policy. Verify a prepared file plan before writes, preserve committed batches on later file changes, and make dry-run use an existing archive read-only without initialization or repair. Existing DMs are not purged; omitted/true retain DM inclusion.

- Reject API history/replies pages with missing or blank top-level message timestamps before page writes or thread work, including periodic repair. Keep earlier pages and the pending retry interval; valid native parent echoes remain supported.

- Report incomplete native MCP history/replies before advancing successful workspace sync state when Slack returns more-page or history/message-limit signals. Keep valid fetched writes and concrete errors; require explicit successful native responses under every DM policy without changing text adapters or tool arguments.

- Reject legacy Git share imports when `sync.include_dms = false`, before acquisition or snapshot/media writes. Subscribe rejects before saving its importing configuration; automatic paths check archive staleness first. Set `share.auto_update = false` to continue local/API work. Existing rows remain unchanged.

- Honor explicit `sync.include_dms = false` before MCP writes using fresh native conversation evidence, including explicit IDs. Text adapters stop before data calls under this policy; native adapters report excluded DMs and leave freshness untouched when nothing is eligible. Apply channel exclusions to every returned alias of a selected ID, and reject selected catalog and retained message/context/thread identity conflicts under every policy while preserving existing payload projections and previously archived rows.

- Keep MCP server response bodies, error text, parser snippets, returned identifiers, and opaque cursors out of ordinary failure diagnostics. Preserve operation/status details, cancellation detection, credential-origin restrictions, and successful intake.

- Honor explicit `sync.include_dms = false` before desktop/wiretap, watch, and all/hybrid desktop writes. Omit unclassified or conflicting cache records and multi-destination drafts with any excluded destination; report omissions and reject retained container identity conflicts before writes. Existing archived rows and omitted/true defaults remain unchanged.

- Honor explicit `sync.include_dms = false` in Socket Mode tailing before message, deletion, or channel metadata writes. Untyped events require conversation read access; lookup failures stop tailing without acknowledging the event. Keep omitted/true DM defaults and restrict channel metadata updates to their owning workspace.

- Report canceled concurrent API syncs as failures while preserving completed writes and the original worker error when it cancels sibling requests.

- Reject successfully authenticated user tokens from another workspace before API sync or tail repair writes; report the mismatch in doctor while preserving bot-only coverage for missing or invalid user tokens.

- Confine directory-export imports to the selected root so symlinks cannot import unrelated files. Compatibility: links outside the root now fail; contained links and a linked export root remain supported.

- Keep digest, quiet-channel and weekly-trend reports within their timestamp windows, and count thread roots independently across channels, including roots identified only by reply metadata.

- Make nested analytics help succeed without a valid configuration and reject non-finite `sync --since` values before opening the archive or starting ingestion.

- Update SQLite's libc runtime to 1.75.7; verify Linux with the race detector and macOS on the minimum Go 1.27.0, provision Node for decoder tests, and pin snapshot packaging to GoReleaser 2.18.2.

- Fix SQL statement validation for comments, quoted identifiers, and named parameters, preventing extra statements hidden by comment-like text. Accept leading comments and reject duplicate result column names with an alias error instead of silently discarding values.

## 0.9.1 - 2026-09-11

**Highlights:** Refresh archive runtime dependencies and security analysis tooling while retaining the Go 1.27.0 minimum.

- Update CrawlKit to 0.16.2, terminal character widths to go-runewidth 0.0.30, and Go network, operating-system, and Unicode text support to x/net 0.59.0, x/sys 0.48.0, and x/text 0.42.0; retain SQLite 1.58.0 with its required libc 1.75.6 runtime.
- Automatically update the Homebrew formula after verified releases and verify its archive checksums before completing the release workflow.
- Update govulncheck to 1.8.0, deadcode to 0.50.0, and the pinned CodeQL action to 4.38.0.
- Update the optional APT/RPM publishing workflows to Cloudsmith CLI 1.27.0.

## 0.9.0 - 2026-09-09

- Migrate archives to schema v8 on first writable open, invalidating pre-v8 API history checkpoints once while retaining messages and retention state. Checkpoints are now local-only in Git shares; restore leaves coverage unknown. The next sync may repeat retention-bounded history requests, or all accessible history without a floor. Stop old processes and keep a consistent pre-upgrade backup; in-place downgrade is unsupported. See [archive schema upgrades](docs/configuration.md#archive-schema-upgrades).
- Keep saved configuration files owner-only (`0600`) on POSIX systems, including existing configs and when subscribing without importing an archive. Saving removes group/other read access.
- Update CrawlKit to v0.15.0 while retaining the Go 1.27.0 minimum and preferred Go 1.27.1 toolchain.
- Retry unfinished API history intervals after partial failures instead of advancing from the newest saved message; legacy and desktop-only histories establish coverage with a retention-bounded backfill.
- Bind automatic MCP Codex credentials to the HTTPS ChatGPT origin and reject credential-bearing redirects outside that origin; dedicated custom-server tokens remain supported.

- Preserve higher-priority API message content, mentions, and deletion state when enriching the archive from Slack Desktop.
- Advertise the normalized `status --json` command in metadata while retaining the legacy global JSON route.

## 0.8.7 - 2026-09-05

**Highlights:** Stop Slack member-directory sync from hanging when `users.list` repeats a page cursor.

### Fixes

- Stop member-directory sync with a clear error when Slack repeats a `users.list` page cursor instead of requesting pages indefinitely. Thanks @SebTardif! (#169)
- Limit release-check HTTP requests to 30 seconds through CrawlKit 0.14.8 so an unresponsive server cannot hang `check-update` indefinitely.

### Maintenance

- Updated the preferred Go build toolchain and container to 1.27.1, Dockerfile frontend to 1.27, CrawlKit to 0.14.9, SQLite driver to 1.58.0, terminal width handling to go-runewidth 0.0.29, and TruffleHog to 3.97.4; the minimum Go version remains 1.27.0.
- Updated the Cloudsmith publishing CLI to 1.26.0.

## v0.8.6 - 2026-08-31

### Fixes

- Stop Slack channel, history, thread, and DM pagination with a clear error when a cursor repeats instead of looping indefinitely. Thanks @SebTardif! (#159)
- Cancel hung Slack Desktop Node decoders when stopping sync, watch, or doctor. Thanks @SebTardif! (#163)
- Stop MCP stdio servers and unblock full stdin pipes when sync is canceled. Thanks @SebTardif! (#164)

### Maintenance

- Updated Go to 1.27.0, SQLite to 1.57.0, Go runtime and test dependencies, Alpine to 3.24, pre-commit hooks to 6.0.0, and CodeQL and TruffleHog action pins.

## v0.8.5 - 2026-08-14

### Maintenance

- Updated the minimum Go toolchain to 1.26.6 to resolve GO-2026-5026, GO-2026-5972, GO-2026-6090, and GO-2026-6218.
## v0.8.4 - 2026-08-13

### Changes

- Local archive queries now open the database strictly read-only (and reopen read-only after share imports), so read commands work on read-only filesystems and mounts. Thanks @rabsef-bicrym! (#145, #146)
- Refined the `metadata` control manifest: Slack-accurate branding, scheduler-friendly `sync` argv, added `search`/`watch` capabilities, and moved it to a tested `controlManifest` helper.

## v0.8.3 - 2026-08-07

### Performance

- Batch Slack API, desktop, MCP, and import ingestion into per-page write transactions (~3.4x faster sync writes).

### Maintenance

- Updated CrawlKit to 0.14.6, fixing the archive TUI filter (typing `q` no longer quits and backspace is rune-safe for CJK/emoji queries) and preventing slow refreshes from stacking overlapping refresh goroutines.

## v0.8.2 - 2026-08-06

### Fixes

- Slack Connect shared channels no longer abort workspace syncs: when a channel already belongs to another workspace in the archive, the bot sync skips it with a warning and keeps syncing the remaining channels instead of failing the whole run.
- Cross-workspace collisions are now skipped for users and messages too, not just channels: an Enterprise Grid user shared between workspaces no longer aborts the sync after every message has already committed, and a shared-channel message no longer terminates `slacrawl tail`.
- `files fetch` no longer discards every attachment when `--max-bytes`/`sync.max_file_bytes` is set to the maximum int64 value: the over-limit probe used to overflow negative and record each file as a fetched empty file.
- `purge` and `publish` now work with a media cache whose root is a symlink, a common setup for large attachment caches on a separate volume. The fetch path always wrote through such a symlink while every read path rejected it, so cleanup failed permanently; symlinks inside the cache tree are still refused.
- `publish` now fails loudly on an unreadable media cache instead of emitting a manifest that claims the archive has no attachments, which silently wiped media for subscribers on their next import.
- `search` and `messages` no longer emit invalid UTF-8 when a message containing emoji or CJK is truncated, and wide characters now count as their display width rather than their byte length.
- Table output stays aligned when cells are empty, `null`, boolean, or non-ASCII; column widths were measured on the ANSI-colorized bytes.
- Cached attachments with fully non-Latin filenames keep their extension, so a published `media/` tree serves them with the right content type.
- Messages that merely quote mention syntax (`&lt;@U…&gt;` in escaped form) are no longer recorded as real mentions in the archive or the `mentions` command.
- Ctrl-C and SIGTERM now cancel long-running commands cleanly instead of hard-killing the process mid-sync.
- `slacrawl tail` survives transient network failures during its periodic repair sweep instead of exiting, its websocket now honors cancellation, and a failing workspace reports its real error instead of an occasional bare `context canceled`.
- `sync --since` accepts RFC3339 timestamps on every backend as documented; previously only the MCP source normalized them and junk values were passed through silently.
- `analytics trends --weeks` rejects absurd values that previously attempted multi-gigabyte allocations.
- MCP stdio responses written just before server exit are no longer occasionally reported as decode errors, and provider subprocesses that die at startup now surface their stderr instead of a bare broken-pipe error.

### Performance

- Thread synchronization no longer scans whole channels per message: a new schema v7 index makes the thread-root lookup O(log n); a 50k-message channel dropped from minutes to milliseconds. Message-event deletes and rendered mention replacement also stopped doing redundant work.

## v0.8.1 - 2026-08-02

### Fixes

- Added a 60-second default Slack API timeout so stalled requests cannot hang the CLI indefinitely. Thanks @SebTardif.

### Documentation

- Rewrote the README around installation and a verified quick start, with detailed command and Git archive references moved to `docs/`.

### Maintenance

- Updated CrawlKit to 0.14.5, SQLite to 1.56.0, and go-colorful to 1.4.1.

## v0.8.0 - 2026-08-02

### Changes

- Added generic external archive providers with an explicit `provider:<name>` sync source, a local JSONL subprocess protocol, scoped resumable checkpoints, bounded validation imports, and source-priority safeguards.

### Fixes

- Made command help independent of local configuration and consistently successful for `--help` and `-h`.
- Added configurable positive row limits to `users` and `channels` while preserving the existing 100-row default.

### Performance

- Batched unchanged-message checks and aligned search-index row IDs during external archive replays to avoid per-message database round trips and full-index replacement scans.

### Maintenance

- Increased the AWS Crabbox root volume to match the current developer image
  snapshot size.
- Provisioned Node.js 24 in Crabbox hydration and made Node-dependent Redux
  decoder tests declare their runtime prerequisite.
- Require explicit workspace, channel, and timestamp scope for live local
  validation instead of embedding workspace-specific defaults.
- Updated govulncheck to 1.6.0 and deadcode to 0.48.0 across local and CI validation.
- Pinned external GitHub Actions to exact reviewed commits while retaining the shared release workflow's `@v1` compatibility contract.
- Standardized the Makefile's build, check, snapshot, and fail-closed release targets across the crawler repositories.
- Refreshed terminal detection and Unicode display-width dependencies.
- Updated CrawlKit to 0.14.4, SQLite to 1.55.0, `golang.org/x/net` to 0.57.0, and replaced the retracted libc 1.74.3 with 1.74.4.
- Updated the stale action to v11 and the GoReleaser action to 7.2.3.
- Aligned releases with the shared OpenClaw Go CLI workflow and disabled Homebrew handoff until Slacrawl has a tap formula.

## v0.7.11 - 2026-07-26

- Re-release v0.7.10's content through the official signed and notarized release pipeline; v0.7.10's macOS archives were signed but not notarized.

## v0.7.10 - 2026-07-26

### Fixes

- Restored wiretap imports from Slack Desktop caches written with V8 wire format 16 (Snappy + Blink v21 envelope), kept older formats working, and made complete IndexedDB decode failures visible instead of reporting an empty successful sync. Thanks @aliou.

## 0.7.9 - 2026-07-20

### Highlights

- Keep archived-message search reliable for malformed Unicode while refreshing the SQLite runtime dependency chain.

### Fixes

- Prevented malformed Unicode in archived messages from hanging search normalization by updating `golang.org/x/text` to 0.40.0. Thanks @dependabot.

### Maintenance

- Update `modernc.org/libc` to v1.74.3, alongside the current `x/sys`, Kong, TruffleHog, setup-go, and CrawlKit v0.14.3 refreshes already landed on main.

## 0.7.8 - 2026-07-18

### Highlights

- Made Git-shared archives resilient to incomplete snapshots: routine updates now merge safely, while explicit restore remains available when an exact replacement is intended.

### Fixes

- Made routine Git-share imports merge-only so destination rows and newer message, user, and channel tombstones survive incomplete snapshots; exact replacement now requires `update --restore`, and removed file and mention rows retain source-attributed tombstones.

### Maintenance

- Migrated releases to the unified OpenClaw pipeline, adding notarized macOS binaries and checksum-bound Debian and RPM packages.
- Updated CrawlKit to 0.14.3, including SQLite 1.54.0 and its related runtime dependency refresh.

## 0.7.7 - 2026-07-09

### Maintenance

- Added fail-closed OpenClaw Developer ID signing and native verification for official macOS release assets while preserving credential-free local and cross-platform builds.
- Updated CrawlKit to 0.13.4.
- Updated the minimum Go toolchain to 1.26.5 to resolve GO-2026-5856 in `crypto/tls`.

## 0.7.6 - 2026-07-06

### Maintenance

- Updated CrawlKit to 0.13.2.
- Updated TruffleHog secret scanning to 3.95.8.

## 0.7.5 - 2026-07-04

### Performance

- Made event-history upgrades constant-time by lazily seeding compact message heads on update instead of scanning the entire archive.

## 0.7.4 - 2026-07-04

### Fixes

- Fixed multi-workspace desktop/API sync scoping, fail-closed workspace authentication, workspace-qualified purges, and explicit watch workspace selection. Thanks @zm2231.
- Prevented unchanged desktop refreshes from duplicating message events and added preview-first retained-history compaction with `purge --keep-message-events`. Thanks @barbieri.
- Normalized relative runtime paths to absolute paths so commands can open databases created with `init --db <relative-path>`.
- Treated nullable optional message metadata as empty when reading historical archives.

### Maintenance

- Hardened release workflows, MCP stdio environment forwarding, command completion, and canonical Homebrew tap targeting.
- Updated CrawlKit to 0.13.1, slack-go to 0.27.0, and `golang.org/x/net` to 0.55.0.
- Corrected Linux package examples to use version-matched GitHub Release assets.

## 0.7.3 - 2026-06-19

### Fixes

- Confined Slack Desktop snapshot reads to the discovered profile root and rejected symlink or special-file entries.

### Maintenance

- Added Bash and Zsh completion entries for Git-share snapshot tags and historical refs.
- Retry concurrent Git snapshot branch-and-tag pushes after rebasing and retargeting the unpublished tag.
- Added immutable Git-share snapshot tags and non-mutating historical restores with `update --ref`, using CrawlKit for shared Git history mechanics.
- Moved FTS5 query escaping onto CrawlKit and refreshed Go dependencies.
- Updated crawlkit through 0.13.0 for shared runtime hardening, SQLite 1.52, and absolute Windows database paths.
- Updated the pinned GoReleaser CI action to 7.2.2.
- Updated the TruffleHog secret-scanning action to 3.95.6.
- Updated GitHub Actions checkout steps to v7.

## 0.7.2 - 2026-06-10

### Changes

- Added automatic Slack Desktop cache discovery on Linux using `XDG_CONFIG_HOME` or `~/.config`. Thanks @TurboTheTurtle.
- Added preview-first retention purging with absolute or relative cutoffs, workspace scoping, cached-media cleanup, and optional SQLite compaction. Thanks @barbieri.

## 0.7.1 - 2026-06-08

### Fixes

- Desktop and wiretap sync now apply configured workspace, channel, and excluded-channel filters to cached Slack Desktop imports.

### Maintenance

- Updated `slack-go` to 0.25.0 while preserving search indexing for rich-text, raw-text, raw-number, and empty table cells.
- Added a pinned dead-code CI gate and removed unreachable internal helpers. Thanks @vincentkoc.
- Updated the TruffleHog secret-scanning action to 3.95.5.

## 0.7.0 - 2026-06-07

### Changes

- Added `sync --source mcp` for fetching Slack users, channels, messages, and threads through Codex's HTTP connector gateway or the reference Slack MCP server over stdio into the canonical SQLite archive.
- Updated the minimum Go toolchain to 1.26.4 to pick up standard-library security fixes.

### Fixes

- MCP sync now handles parent-only thread payloads, replies with missing author names, missing Slack connectors, and shell completion for `mcp`/`connector` sources.

## 0.6.3 - 2026-05-25

### Fixes

- Search now treats normal CLI input as safe text instead of raw FTS syntax, with phrase, term, and substring fallback plus `--raw-fts` for advanced queries.
- Wiretap desktop import now includes cached Slack DM and MPIM messages from IndexedDB redux state.
- Desktop ingest now detects direct-download Slack installs and skips desktop-only cross-workspace collisions from IndexedDB snapshots. Thanks @caocuong2404.

## 0.6.2 - 2026-05-18

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
