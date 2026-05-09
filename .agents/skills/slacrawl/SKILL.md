---
name: slacrawl
description: "Slack archive: search, sync, threads/DMs, Slacrawl repo work."
---

# Slacrawl

Use local Slack archive data first. API sync/full threads/DMs require Slack tokens; desktop mode can work without tokens.

## Sources

- DB: `~/.slacrawl/slacrawl.db`
- Repo: `~/Projects/slacrawl`
- CLI: `slacrawl`
- Typo shim: `slacawl`

## Freshness

For recent/current Slack questions:

```bash
slacrawl doctor
slacrawl status --json
```

Desktop-only refresh:

```bash
slacrawl sync --source desktop
```

API latest refresh, when tokens are available:

```bash
slacrawl sync --source api --latest-only
```

Use `--full` only for deliberate historical backfills.

## Query Workflow

1. Resolve workspace, channel/DM, date range, user, and keyword.
2. Check freshness if the question is recent/current.
3. Prefer CLI search/messages for slices; use read-only SQL for exact counts.
4. Report workspace/channel names, date spans, counts, and token/source limits.

Common commands:

```bash
slacrawl search "query"
slacrawl messages --since 7d --limit 50
slacrawl sql 'select count(*) from messages;'
```

## Verification

For repo edits:

```bash
go test ./...
make test
```

Use a small CLI smoke such as:

```bash
slacrawl doctor
slacrawl search test --limit 5
```
