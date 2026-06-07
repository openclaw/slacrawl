---
name: slacrawl
description: "Slack archive: search, sync, threads/DMs, Slacrawl repo work."
---

# Slacrawl

Use local Slack archive data first. Hit Slack APIs only when archive is stale, missing scope, or user asks for current external context.

## Sources

- DB: `~/.slacrawl/slacrawl.db`
- Config: `~/.slacrawl/config.toml`
- Cache/logs: `~/.slacrawl/{cache,logs}`
- Git share repo: `~/.slacrawl/share`
- Repo: `~/Projects/slacrawl`
- CLI: `slacrawl`

## Freshness

For recent/current Slack questions:

```bash
slacrawl doctor
slacrawl status --json
```

Refresh:

```bash
slacrawl sync --source bot --latest-only
slacrawl sync --source mcp --workspace T01234567
slacrawl sync --source wiretap
```

Use `--full` only for deliberate historical backfills. `bot` = API tokens; `mcp` = configured HTTP or stdio Slack connector; `wiretap` = Slack Desktop cache; `all` = API then desktop enrichment.

## Query Workflow

1. Resolve workspace, channel/DM, date range, user, and keyword.
2. Check freshness if the question is recent/current.
3. Prefer CLI search/messages for slices; use read-only SQL for exact counts.
4. Report workspace/channel names, date spans, counts, and token/source limits.

Use root or subcommand help for syntax: `slacrawl --help`,
`slacrawl search --help`, `slacrawl messages --help`, `slacrawl sql --help`.
`slacrawl search --limit N` is supported.

Common commands:

```bash
slacrawl search --limit 20 "query"
slacrawl messages --since 7d --limit 50
slacrawl channels --json
slacrawl users --json
slacrawl mentions --limit 50
slacrawl sql 'select count(*) from messages;'
```

Use `slacrawl --json sql ...` for exact read-only counts/joins/rankings. Keep SQL to `select`/`with`.

## Verification

For repo edits:

```bash
GOWORK=off go test ./...
make test
```

Use a small CLI smoke such as:

```bash
slacrawl doctor
slacrawl search test
```
