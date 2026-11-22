---
name: taskmoor
description: Use when a repository keeps its tasks as files (tasks/*.md + tasks/tasks.toml, the taskmoor task format) — to find a takeable task, read one, check task files before committing, and change a task's lifecycle only in the ways an agent may.
---

# taskmoor

The tasks live in `tasks/<ID>-<slug>.md` (YAML front matter + body) and
`tasks/tasks.toml`. The format is `spec/README.md` in the taskmoor
repository, `format = 1`. Use the `taskmoor` CLI; do not parse the files by
hand.

## Find

- `taskmoor ready` — tasks that can start now, priority then id. A task
  marked `⚑` needs a person: do not start it unless a person started it or
  permitted this run.
- `taskmoor list` — every task with its lifecycle (`backlog`, `in_progress`,
  `review`, `merged`, `done`, `cancelled`).
- Add `--json` for machine-readable output (`"format": 1`).

## Read

- `taskmoor show ID` — the front matter, the lifecycle, why it is not
  ready, the state (branch, worktree, base), the harness status and the body.
- Work to the body's **Acceptance** list; run what **Checks** says; keep to
  **Limits**; the `reviews` field is the most review rounds allowed.

## Check

- `taskmoor validate` before committing a change under `tasks/` — it reads
  the working tree. Exit 1 means errors; fix them, do not commit around them.

## What you may write

- The task file on your task branch: tick Acceptance boxes as they are met;
  record a disagreement with the task in the file instead of coding around it.
- The lifecycle only through the CLI, and only when you are the launcher or
  were asked to: `taskmoor claim ID --branch B --worktree W` (start),
  `taskmoor review ID` (work finished, waiting for review),
  `taskmoor release ID` (back to backlog).

## What you may not write

- Your own harness status — whoever runs the harness reports it.
- The state file under `$(git rev-parse --git-common-dir)/tasks/state/` by
  hand.
- `status` in a task file: `backlog` → `cancelled` is a person's decision on
  the main branch.

## Merging

A task is `merged` when one commit on the main branch's first-parent history
ends with the trailer `Task: <ID>` in its last paragraph (the merge commit,
or the squash commit). One task, one such commit.
