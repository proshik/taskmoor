---
id: TM-002
title: Options for watching and joining a running session's terminal
kind: research
status: backlog
priority: 3
component: docs
depends_on: []
touches:
  - docs/research/live-terminal.md
harness: any
reviews: {spec: 0, code: 0}
created: "2026-11-24"
---

## Goal

"Attaching to a background session's live terminal" is the last open item
of Later in `docs/conveyor.md` (C11). Sessions run headless today: Claude
as `claude -p` stream-json, Codex and OpenCode over ACP; the person sees
the feed on the board and can take a session over with Continue in
terminal, which ends it in the service.

When this task is finished, `docs/research/live-terminal.md` lays out the
ways the person could watch a running session as it works and type into
it without taking it over, for each harness, so the person can choose one.

## Acceptance

- For Claude, Codex and OpenCode: what each one offers for this today
  (a PTY mode, a resumable session another process can join, an API for
  input mid-turn), with the source for each claim (docs, `--help`, code).
- Two or three concrete designs (for example: a PTY per session the
  service owns and the web streams; the feed plus a mid-turn input
  channel; handing the session to a terminal and back), each with what it
  changes in the service, the rules and the sandbox, and its risks.
- A recommendation and the questions only the person can answer.
- Nothing outside `docs/research/` changes.

## Checks

- The document's claims cite a source; commands named in it exist in the
  installed versions (`claude --version`, `codex --version`).

## Limits

- Research only: no code, no new dependencies, nothing installed.
