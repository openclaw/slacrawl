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

When `workspace_id` is set in configuration, purge uses that workspace by
default. Pass `--workspace` for a different workspace or `--all-workspaces` for
an explicit archive-wide purge.

Workspace-scoped purge carries `workspace_id` through its temporary selection
and deletion set. The archive schema still treats Slack `channel_id` plus `ts`
as the persistent message identity, so imports and syncs must not create two
messages with the same `channel_id` and `ts` in different workspaces.

Pass `--force` to execute:

```bash
slacrawl purge --workspace T01234567 --older-than 90d --force
```

The SQLite transaction deletes:

- messages
- message event history
- file metadata
- extracted mentions
- embedding jobs
- FTS entries

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

Purge changes the local database only. Publish a new snapshot after purging if
other readers should receive the retention change. Importing an older snapshot
can restore records that were removed locally.
