---
id: TM-004
title: A session's sandbox settings file is not loaded as a session
kind: fix
status: backlog
priority: 1
component: core
depends_on: []
touches:
  - internal/serve/journal.go
  - internal/serve/journal_test.go
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-25"
---

## Goal

`journal.sessions()` reads every `*.json` in the store's `sessions/`
directory as a session record. A Claude session there also has
`<id>.settings.json` (its sandbox settings), which decodes into an empty
session: after a restart of `taskmoor serve`, `taskmoor sessions` lists a
row with no id, task or profile, `stopped`, `UNCONFINED` (seen 2026-11-25
with two Claude sessions in the store).

When this task is finished, only session records are loaded, and a file
that is not one is skipped, not turned into a session.

## Acceptance

- `journal.sessions()` loads `<id>.json` only (a name whose stem is a
  session id, the record's `id` equal to it); `*.settings.json` and any
  other file are skipped.
- A record whose `id` is empty or does not match its file name is skipped
  with a warning in the service log, not loaded.
- A test writes a session, its settings file and a stray JSON file into a
  journal directory and shows that only the session loads; a test through
  a service restart shows `taskmoor sessions` without the empty row.

## Checks

- `go test -race -count=1 ./internal/serve/ -run Journal`
- `go vet ./... && gofmt -l .`

## Limits

- No change to where the settings file is written or what it holds.
