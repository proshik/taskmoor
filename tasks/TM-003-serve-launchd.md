---
id: TM-003
title: Run taskmoor serve as a launchd agent
kind: feature
status: backlog
priority: 3
component: core
depends_on: []
touches:
  - internal/cli/servecmds.go
  - internal/cli/serve_test.go
  - README.md
  - docs/conveyor.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-24"
needs_person: true
---

## Goal

`taskmoor serve` runs in the foreground of a terminal tab ("launchd
later" in `docs/conveyor.md`); closing the tab or logging out stops the
conveyor. The person wants it to start at login and restart if it exits,
the way stage 9's tunnel agent already does.

When this task is finished, `taskmoor serve install` writes a launchd
agent for the service and says how to load it; `taskmoor serve uninstall`
removes it.

## Acceptance

- `taskmoor serve install [--dry-run]` writes
  `~/Library/LaunchAgents/<label>.plist` (RunAtLoad, KeepAlive on failure,
  the current PATH and TASKMOOR_HOME if set, logs in the store) and prints
  the `launchctl bootstrap gui/$UID …` command; it does not run launchctl
  itself. An existing different plist is not overwritten without `--force`.
- `taskmoor serve uninstall` prints the `launchctl bootout` command and
  removes the plist it wrote (and only that).
- The sign-in link of a service started this way: `taskmoor web` opens a
  new one (documented).
- Tests write into a temporary HOME; no test runs launchctl.
- README and `docs/conveyor.md` (a numbered decision) describe it.

## Checks

- `go test -race -count=1 ./internal/cli/`
- `go vet ./... && gofmt -l .`

## Limits

- Never runs `launchctl`, never touches agents it did not write.
- A new user-visible command: the person starts or permits this run (⚑).
