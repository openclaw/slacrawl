# Retention Purge

`slacrawl purge` previews or removes messages older than an exclusive cutoff.
It is intended for archive-size management and local retention policies.
Threads use their parent timestamp, so an expired parent and all of its replies
are purged together even when a reply itself is newer than the cutoff.
Unsent desktop drafts are independent records and use their own draft timestamp
or last update time, including drafts that reply to an expired thread.
Legacy draft update times stored at whole-second precision are retained when
they fall within the cutoff second rather than risking deletion of a newer draft.

Choose one cutoff:

```bash
slacrawl purge --older-than 90d
slacrawl purge --before 2026-01-01
slacrawl purge --before 2026-01-01T12:00:00Z
```

Dates without a time use midnight UTC. Relative durations accept Go duration
syntax such as `2160h` plus the `Nd` day shorthand.

## Safety

Preview is the default. A preview reports the affected messages, message-owned
rows, and unreferenced cached media without changing the archive:

```bash
slacrawl --json purge --workspace T01234567 --older-than 90d
```

Without `--workspace`, purge applies archive-wide across every workspace. Pass
`--workspace` to scope it to a single workspace.

Workspace-scoped purge carries `workspace_id` through its temporary selection
and deletion set. The archive schema still treats Slack `channel_id` plus `ts`
as the persistent message identity, so imports and syncs must not create two
messages with the same `channel_id` and `ts` in different workspaces.

Pass `--force` to execute:

```bash
slacrawl purge --workspace T01234567 --older-than 90d --force
```

Retained messages can also accumulate historical snapshots. Add
`--keep-message-events N` to preview keeping only the newest `N` events for
each message, event type, and source:

```bash
slacrawl --json purge --older-than 90d --keep-message-events 5
slacrawl purge --older-than 90d --keep-message-events 5 --force --vacuum
```

The preview reports `compacted_message_events` without changing the database.
Compaction never removes the canonical current row in `messages`, does not mix
event sources or event types, and follows `--workspace` when supplied.

The SQLite transaction deletes:

- messages
- message event history
- file metadata
- extracted mentions
- embedding jobs
- FTS entries

Consecutive identical message snapshots are suppressed during normal ingest;
real transitions, including a value changing and later reverting, remain in
event history.

When a complete message payload no longer contains a previously stored file or
mention, Slacrawl retains that subordinate row with `deleted_at`,
`deletion_source`, and `deletion_reason`. An explicit parent-message delete does
the same. Normal file, mention, digest, and search reads exclude those
tombstones; a later authoritative payload can resurrect the same stable row.

Workspaces, channels, users, and sync state remain. Executed purges also record
a per-channel retention floor so ordinary incremental API and MCP syncs do not
restore deleted history through their repair overlap. New replies to expired
thread parents are also excluded by ordinary incremental ingestion. An explicit
full sync, an older `--since` value, desktop ingestion, or import can restore
purged history when the source still exposes it.

Cached media with no remaining database reference is removed by default. Use
`--keep-media` when the binary cache must remain on disk:

```bash
slacrawl purge --older-than 90d --force --keep-media
```

Each forced cleanup scans the cache for unreferenced files, so a later purge
retries files left behind by an earlier filesystem error.

## Database Size

SQLite reuses pages freed by deletion, but the database file usually keeps its
current filesystem size. Use `--vacuum` to compact it immediately:

```bash
slacrawl purge --older-than 90d --force --vacuum
```

`VACUUM` requires additional temporary disk space and holds an exclusive
database operation while it rebuilds the file.

## Git-Backed Archives

Purge changes the local database only. Publishing a snapshot after purging does
not delete rows on normal readers because absence from a snapshot is not a
deletion signal. Use an explicit `update --restore` on a reader when it must
exactly adopt the published retention set. A historical restore uses
`update --restore --ref <tag-or-commit>` and can bring back records removed
locally.
