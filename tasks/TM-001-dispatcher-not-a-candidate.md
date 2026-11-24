---
id: TM-001
title: The dispatcher's own profile is not offered as a task's profile
kind: fix
status: backlog
priority: 2
component: core
depends_on: []
touches:
  - internal/serve/suggest.go
  - internal/serve/suggest_test.go
  - docs/conveyor.md
harness: any
reviews: {spec: 0, code: 1}
created: "2026-11-24"
---

## Goal

When the person asks the dispatcher which profile should run a task
(stage 13, "Ask the dispatcher"), the candidates it chooses from include
the profile named by `dispatcher_profile` itself, so it can suggest
running a whole task on the profile meant for its own short calls. That
profile is configured for the dispatcher (often a cheaper model, no
description), not for task work.

When this task is finished, the dispatcher never offers its own profile as
a candidate; the person can still pick it by hand.

## Acceptance

- `suggestCandidates` leaves out the profile named by `dispatcher_profile`;
  the snapshot's `suggestable` counts candidates without it (so a config
  with one task profile and the dispatcher's shows no Ask button).
- A test in `internal/serve/suggest_test.go` shows the dispatcher's profile
  absent from the prompt and refused if the reply names it.
- `docs/conveyor.md`, Stage 13 decision 2, says the dispatcher's own
  profile is not a candidate.

## Checks

- `go test -race -count=1 ./internal/serve/ -run Suggest`
- `go vet ./... && gofmt -l .`

## Limits

- Only the candidate list changes; Run, the routing order and a manual
  pick stay as they are.
