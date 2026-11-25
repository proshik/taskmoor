---
id: TM-005
title: A profile can let its agent's tests listen on local ports
kind: feature
status: backlog
priority: 2
component: core
depends_on: []
touches:
  - internal/serve/config.go
  - internal/serve/sandbox.go
  - internal/serve/sandbox_test.go
  - README.md
  - docs/conveyor.md
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-25"
---

## Goal

Claude's sandbox for sessions (C13, `sandbox.go`) does not allow binding
local ports. Tests that start a server on `127.0.0.1:0` fail there with
`listen tcp 127.0.0.1:0: bind: operation not permitted`: TM-001 and TM-004
both committed tests their agent could not run, and the person ran them
outside the sandbox. Most Go, Node and Python projects have such tests.

When this task is finished, a profile can say `allow_local_binding =
true`, and that profile's sessions can listen on and connect to loopback
ports for their tests, while everything else the sandbox confines stays
confined.

## Acceptance

- `Profile.AllowLocalBinding` (`allow_local_binding`, default false) in
  `config.go`; when true, the session's sandbox settings set
  `sandbox.network.allowLocalBinding: true` and allow connections to the
  loopback (whatever Claude Code's sandbox needs for a test to reach its
  own listener: check the current settings schema and say which keys).
- The limits that must not weaken (AGENTS.md) hold, each shown by a test
  of the generated settings: the store (with the API token) and the
  credentials stay unreadable, other hosts stay refused
  (`strictAllowlist`), and nothing else in the settings changes when the
  option is off.
- The service's own listeners are not a way out: with the token
  unreadable, an agent reaching `127.0.0.1:<port>` of `taskmoor serve`
  gets 401; say so in the decision, with the test that shows the token
  path is denied.
- The board's profile list and `taskmoor` profile output show the option
  where they show the sandbox.
- A real check, recorded in `docs/conveyor.md` (a numbered decision):
  a session on a profile with the option runs `go test ./internal/serve/
  -run TestRunAQuestionAndCompletion` in its worktree and it passes;
  without the option it fails with the bind error.
- README: the option, what it opens and what stays confined.

## Checks

- `go test -race -count=1 ./internal/serve/ -run 'Sandbox|Config'`
- `go vet ./... && gofmt -l .`

## Limits

- Off by default; no other sandbox setting changes.
- Never `allowUnsandboxedCommands`, never a wider domain list.
