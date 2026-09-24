# Desktop Mode

Desktop mode lets `slacrawl` ingest local Slack Desktop state into SQLite without depending on Slack search.

This path is read-only. It snapshots Slack Desktop artifacts, parses supported local state, and upserts what it can recover into the database.

Desktop snapshots use shared safe read-only cache helpers where possible.
Slack-specific IndexedDB/Local Storage parsing and merge policy remain in
`slacrawl`, and the tool never writes to Slack application storage.

## What Desktop Mode Ingests

Today the desktop adapter can ingest:

- workspace metadata from local desktop state
- cached public and private channel metadata
- cached user/member profiles
- cached channel, DM, and MPIM message history recovered from IndexedDB redux persistence blobs
- cached thread roots and cached thread replies recovered from IndexedDB redux persistence blobs when Slack Desktop has them
- draft messages, unless `[slack.desktop].include_drafts = false`
- read markers from local persisted API calls
- custom status metadata
- object store inventory for IndexedDB drift tracking

Message attachments, including quoted messages and link unfurls, remain in the
parent message's raw payload. They are not discovered as separate messages,
even when they contain timestamps and another channel ID. DM exclusion applies
to the parent conversation; it does not redact quoted content inside attachments.

The desktop adapter intentionally does not use local desktop auth material for write actions.

To recover sent messages without archiving unsent drafts, set
`include_drafts = false` under `[slack.desktop]`. The setting applies to
desktop/wiretap sync, `watch`, and the desktop phase of all/hybrid sync. It
excludes draft-derived messages, channel hints, raw payloads, event history,
search entries, and sync draft counts before persistence. Existing archived
drafts remain unchanged. Desktop snapshots still read the cache; this setting
does not remove DMs or make an existing archive safe to publish.

## Excluding Direct Messages

Set `[sync].include_dms = false` to exclude future desktop DM intake. The policy
applies to desktop/wiretap sync, `watch`, and the desktop phase of all/hybrid
sync. Omitted/true retain the existing desktop recovery behavior.

Strict exclusion requires selected cache metadata that identifies a public or
private channel. IM/MPIM flags take exclusion precedence; ID prefixes and the
private flag alone do not prove a channel type. Conflicting or unknown type
observations from other decoded blobs veto admission, even when a richer blob
is selected. Historical metadata cannot supply missing selected classification.

Admission runs before desktop writes. It covers channel metadata, cached
messages, draft-derived rows, recent-channel hints, and read-marker checkpoints.
Every destination in a draft must be eligible and resolve to the same workspace;
otherwise the whole draft is omitted. A retained channel/message identity that
conflicts with its cache container stops the desktop phase before any writes.
Workspace and channel selectors still apply.

Sync reports **Completed with omissions** and fixed reason counts when records
are excluded or decoding is partial. With strict exclusion, unknown or heuristic
message shapes and unattributed download/expandable counts are omitted. Missing
Node prevents positive cached-channel classification. `doctor` continues to show
raw cache diagnostics; member profiles and custom statuses remain independent
metadata and are not anonymized by this policy.

This admission policy does not purge previously archived rows or certify
historical DM-origin content. Persistent channel hints retain their existing
identity model. Neither an admitted current channel type nor these controls
certify a safe export.

## Retention After Purge

Sent messages recovered from Redux caches honor the current global, workspace,
and channel retention floors when each message batch is written. Replaying a
cache cannot reinsert a purged sent message below the strongest applicable
floor, including when cache preparation happened before the purge. Replies use
their parent timestamp; a newer reply to an expired root is also omitted.
Messages at the cutoff remain eligible. An exact message row already present
below the floor can still receive updates under the usual source-priority rules.

This applies to desktop/wiretap sync, `watch`, and the desktop phase of all/hybrid
sync. Workspace/channel/profile metadata, inventory and checkpoints may still
refresh. Admission counts describe prepared input, not newly inserted rows.
Draft retention remains separate; this sent-message rule does not prevent
purged drafts from returning.

## Read-Marker Checkpoint Identity

New intake stores read markers in `sync_state` with source `desktop`, entity
type `read_marker_v1`, and a compact JSON `[workspace_id,channel_id]` key. The
workspace is the owner resolved during admission, which can differ from the
persisted call's workspace. The value remains the original timestamp string.

Users and persisted calls within the same workspace/channel still overwrite
one shared value in ingestion order. Calls from an unsorted map have no
guaranteed winner; this is neither a maximum timestamp nor per-user read state.

Legacy `read_marker` channel-only rows remain historical evidence. New intake
does not read, attribute, migrate, update or delete them. SQL consumers of new
markers must use the versioned namespace and tuple key. Channel metadata cannot
safely identify the owner of an old marker.

Both namespaces retain normal snapshot behavior: export carries them, merge
keeps existing local conflicts, and Restore replaces rows from the snapshot.
Message purge does not clear these markers. Marker counts still describe
admitted calls, and writes keep their existing contribution to archive freshness;
neither counts nor markers certify historical message coverage.

## What It Does Not Yet Cover

Desktop mode is still partial in a few areas:

- Slack Desktop only exposes conversations it has cached locally, so recent or opened DMs/MPIMs are more likely to import than cold history
- attachment blobs are not downloaded
- background file/media caches are not indexed as searchable attachments

Git sharing can back up media already downloaded through the Slack API. Desktop
ingestion itself does not download blobs. See [Git archive sharing](git-archive-sharing.md).

## Path Detection

Leave the desktop path blank to auto-detect a supported Slack Desktop install:

```toml
[slack.desktop]
enabled = true
path = ""
```

The supported default targets are:

```text
# macOS
~/Library/Containers/com.tinyspeck.slackmacgap/Data/Library/Application Support/Slack

# Linux
${XDG_CONFIG_HOME}/Slack
~/.config/Slack
```

If your Slack Desktop data lives elsewhere, set the path explicitly.

## One-Shot Desktop Sync

Run a one-shot desktop import:

```bash
slacrawl sync --source desktop
```

`wiretap` is the human-readable alias for desktop mode:

```bash
slacrawl sync --source wiretap
```

This will:

1. snapshot the local Slack Desktop storage
2. parse Local Storage and IndexedDB data
3. merge supported rows into SQLite

Ctrl-C cancels desktop sync, watch, and doctor, including a Node decoder that stops responding.

When upgrading from pre-v0.9.0, stop archive writers and keep a consistent SQLite
backup before the first sync. Opening the archive writable upgrades it to schema
8 even if desktop sync later fails. Older binaries cannot open that upgraded
archive; rollback requires restoring the pre-upgrade backup. See
[Archive Schema Upgrades](configuration.md#archive-schema-upgrades) for recovery
and backup guidance.

## Continuous Desktop Refresh

Use `watch` to keep refreshing the DB from local desktop state:

```bash
slacrawl watch --desktop-every 5m
```

This loop does not truncate the database. It repeatedly upserts desktop state and appends event history when a message snapshot changes, so unchanged refreshes do not amplify the archive. It refreshes every workspace in the signed-in desktop profile by default; pass `--workspace T01234567` to restrict each refresh to one workspace.

## Validation Commands

After a desktop sync, the most useful checks are:

```bash
slacrawl doctor
slacrawl status
slacrawl channels
slacrawl users
slacrawl messages --limit 20
slacrawl sql "select entity_id, value from sync_state where source_name = 'desktop' order by entity_type, entity_id"
```

## Operational Notes

- Rich IndexedDB redux blob decoding uses `node` when available.
- If `node` is unavailable, desktop sync still runs, but decoded cached channel/user/message coverage will be reduced.
- IndexedDB redux blobs are decoded per blob across known framings: bare V8 wire format 15/16, Slack Snappy wrapping, historical split Blink/V8 headers, and the current Snappy + Blink v21 envelope. V8 v16 payloads use an explicit v16-to-v15 compatibility adaptation; unknown future versions are reported as unsupported and never relabeled.
- `doctor` reports IndexedDB blob totals, recognized candidates, decoded counts, detected V8 versions, and decode failures grouped by stage. Desktop sync fails rather than reporting success when every recognized candidate fails to decode.
- Desktop data is merged at lower precedence than API data.
- Re-running desktop sync is safe; canonical rows are upserted by Slack-native keys.
