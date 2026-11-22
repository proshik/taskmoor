# The taskmoor task format — `format = 1`

A task is one file in a repository's `tasks/` directory. One file is one
finished, independently takeable piece of work: **one worktree, one branch,
one merge**. What a task is lives in git beside the code it changes; how far
it has got lives outside git, in a state directory every worktree shares.

The format depends on no tool. It is compatible in spirit with Backlog.md —
Markdown with a YAML front matter, kept in the repository — and nothing in it
is specific to one project or one harness. This document, `task.schema.json`
and `state.schema.json` are the format's authoritative definition; taskmoor
(this repository) is a reference reader and writer of it. A project adopts the
format by writing a `tasks.toml` of its own (see `tasks.example.toml`) and its
task files; it may keep pinned copies of the schemas beside them, with a note
that these are authoritative.

## Layout

```
tasks/
  tasks.toml           the project: id prefix, main branch, components, release rules, routing
  <ID>-<slug>.md       the tasks
  README.md            optional: the project's own notes; not a task
```

The task directory is `tasks/` at the repository's root; format 1 fixes the
name. Every `*.md` file directly in it is a task file except `README.md`;
other files and subdirectories are ignored by readers.

A task's file name is its `id`, a hyphen and a short lowercase slug:
`PRJ-007-light-start-settings.md`. The `id` in the front matter must equal
the file name's prefix; the slug may change, the `id` never does, and an `id`
is never reused — a task that is dropped is `cancelled`, not deleted.

## A task file

```markdown
---
id: PRJ-007
title: Heavy features off by default behind settings
kind: feature
status: backlog
priority: 2
component: desktop
depends_on: [PRJ-006]
touches:
  - src/app/lsp.rs
  - src/settings/**
harness: any
reviews: {spec: 0, code: 2}
created: "2026-11-07"
---

## Goal
## Acceptance
## Checks
## Limits / non-goals
## Context
```

Machines read the front matter; people and agents read the body.
`task.schema.json` pins the front matter, with `additionalProperties: false`,
so a tool rejects a misspelt key instead of ignoring it.

The front matter is read as JSON-compatible YAML: strings, integers,
booleans, lists and maps. **Dates and commits are quoted**
(`created: "2026-11-07"`, `after_release: "0068f472"`): an unquoted date is a
timestamp to most YAML parsers, an all-digit commit prefix is a number, and
both fail the schema's `string`.

### Fields

| Field | Required | Meaning |
| --- | --- | --- |
| `id` | yes | `<PREFIX>-<number>`, three digits at least; the prefix is `tasks.toml`'s `id_prefix`. |
| `title` | yes | What is done, as a sentence fragment a person reads in a list. |
| `kind` | yes | `feature`, `fix`, `chore` (refactor, cleanup, tooling, documents), `research` (finding something out and recording it), `design` (a decision recorded, no production code). |
| `status` | yes | `backlog` or `cancelled` — only what a person decides on the main branch. See [Status](#status). |
| `priority` | yes | 1 (highest) to 4. |
| `component` | yes | A component named in `tasks.toml`; it decides which release rule makes the task `done`. |
| `depends_on` | yes | Ids of tasks that must be `merged` before this one starts. An empty list is written `[]`. |
| `touches` | yes | Paths and globs the task is expected to change. Two tasks whose `touches` overlap are not run at the same time. |
| `harness` | yes | The harness preferred for the work: `any`, `claude`, `codex`, `opencode`. A preference, not a constraint. |
| `reviews` | yes | `{spec: n, code: n}` — the most independent review rounds allowed for the task's plan and for its code. `0` means none. A failed round counts. |
| `created` | yes | The day the task was written, `"YYYY-MM-DD"`. |
| `needs_person` | no | `true` when the task cannot be finished unattended: it needs a person's explicit permission (provider calls, writes to their accounts) or a person at the machine (a live window check). Default `false`. |
| `after_release` | no | A commit on the main branch that must be contained in a release of the task's component before the task starts — for work that waits on a release rather than on another task. |
| `profile` | no | The name of an agent profile (`^[a-z][a-z0-9-]*$`) to run the task with; overrides `tasks.toml`'s `[routing]`. Profile names are defined by the launcher's configuration, not by the format; a reader that does not launch agents only checks the name's shape. |
| `model` | no | A model name that overrides the chosen profile's model for this task. |

`needs_person` and `after_release` were added to the fields first proposed
because two of the first tasks could not be described without them: a live
check against real harnesses is not something an agent may start on its own,
and archiving a shipped plan waits on a release, which is not a task.

`harness` and `profile` are both preferences for a launcher. An explicit
`profile` wins; otherwise a `harness` other than `any` narrows the routed
choice to profiles of that harness. A reader that launches nothing only
validates them.

### Body

- **Goal** — what is different when the task is finished, and why.
- **Acceptance** — a checkbox list, each item a WHEN/THEN that someone can
  check without reading the author's mind. A task is finished when every box
  is ticked.
- **Checks** — what is run to prove it: tests, `just check`, a measurement, a
  window check — and what is deliberately not run.
- **Limits / non-goals** — what the task must not do, and the deferred work
  that bounds it.
- **Context** — links to code, OpenSpec changes, decisions and commits.

A body states facts about the code with a path and line where it can, and
marks what the author has not verified.

## Status

A task has two statuses, which answer different questions and come from
different places.

### Lifecycle — how far the task has got

| Value | Meaning | Source |
| --- | --- | --- |
| `backlog` | Written, not started. | The file's `status`, on the main branch. |
| `cancelled` | Dropped by a person. | The file's `status`, on the main branch. |
| `in_progress` | Someone is working on it in a worktree. | The shared state file. |
| `review` | Its work is finished and waiting for review or merge. | The shared state file. |
| `merged` | Its branch is in the main branch. | Git: a commit on the main branch's first-parent history carrying the trailer `Task: <ID>`. |
| `done` | It has shipped. | Git: a release tag of its component (per `tasks.toml`) contains that commit. |

A reader works the lifecycle out in this order, the first match winning:

1. the file on the main branch says `cancelled` → `cancelled`;
2. a release of the component contains the task's merge commit → `done`;
3. the main branch carries the task's merge commit → `merged`;
4. the state file says `in_progress` or `review` → that;
5. otherwise → `backlog`.

Derived facts win over written ones: once git shows the merge (or the file
says `cancelled`), a state file still saying `in_progress` or `review` is
**stale**. A reader shows it as stale and never deletes it; removing it is a
writer's act (`taskmoor release`).

**Why the changing part is not in the file.** An agent works in its own
worktree on its own branch. A status written into the task file there is
invisible to a board reading the main branch until the merge, and every
parallel agent touching `tasks/` collides with every other in the merge. So
the file holds only what a person decides on the main branch, git answers
`merged` and `done` by itself, and the rest is in a state directory shared by
all worktrees and never committed.

### Harness — what the agent's program is doing now

| Value | Meaning |
| --- | --- |
| `idle` | The program is waiting for input, not in a turn. |
| `working` | A turn is running. |
| `blocked` | The program waits for a person; `kind` says why when the program reports it: `permission`, `question` or `auth`. An unstated reason stays `blocked` with no `kind`, never a guessed one. |
| `done` | The turn finished. |
| `error` | The turn ended in an error. |
| `unknown` | The program reports nothing (no OSC 7501), or has not reported yet. |
| `exited` | The program's process ended. |

The first five are the states of OSC 7501
(https://docs.otty.sh/vt/osc/osc-7501) — `error` is the protocol's too, and
is kept rather than folded into another state, so nothing a program reports
is lost. `unknown` and `exited` are what the terminal itself knows about a
program that does not report, or no longer can.

The harness status is the terminal's OSC 7501 root record, **carried by
whoever runs the harness** — the launcher or the terminal owning the
program's PTY — into the state file. An agent never writes its own status:
self-reports are what OSC 7501 replaced.

## The shared state file

```
$(git rev-parse --git-common-dir)/tasks/state/<ID>.json
```

`--git-common-dir` is the same directory from every worktree of a
repository, so the main checkout and every task worktree see one file per
task. It is inside `.git`, so it is never committed. `state.schema.json`
pins its shape:

```json
{
  "format": 1,
  "id": "PRJ-007",
  "lifecycle": "in_progress",
  "worktree": "/Users/someone/work/project-gld-007",
  "branch": "task/PRJ-007-light-start-settings",
  "base": "0068f47258fed459a8825469a44e6344de1f96e6",
  "started_at": "2026-11-08T10:12:00Z",
  "updated_at": "2026-11-08T10:40:31Z",
  "writer": "taskmoor 0.1.0",
  "harness": {
    "name": "claude",
    "status": "blocked",
    "kind": "permission",
    "observed_at": "2026-11-08T10:40:31Z",
    "session": "7c0c9a4a-1c92-4a6a-bfe4-c25d721fa60e"
  }
}
```

- `lifecycle` is only ever `in_progress` or `review`: everything else comes
  from the file or from git.
- `harness` is absent before a harness is launched; `kind` is present only
  with `blocked`, and only when reported; `exit_code` may follow `exited`.
- `harness.session` is the harness's own id of the running session, when the
  launcher knows it. A reader looking for the running program matches it by
  `session` first and by `worktree` only without one.
- **Written by the launcher or the CLI, never by the agent.** One writer per
  task at a time. A write goes to a temporary file in the same directory and
  is published from there, so a reader sees one whole version or the
  previous one: a new file by `link(2)` (atomic create-if-absent, so two
  concurrent starts cannot both win), a replacement by `rename(2)`.
- Writers that start tasks re-check readiness and publish the new file under
  one per-project lock, `flock(2)` on `<state directory>/.lock`, so two
  starts of different tasks whose `touches` overlap cannot both succeed.
- A reader treats a missing, unreadable or schema-invalid file as no state
  (lifecycle from the file and git alone) and says so, rather than guessing.
- `taskmoor claim`, `review` and `release` are writers of this file.

## The `Task:` trailer

A task is merged by one commit on the main branch whose message ends with
the git trailer

```
Task: PRJ-007
```

in its last paragraph (`git interpret-trailers` reads it). With a merge
commit that is the merge; with a squash or a fast-forward of one commit it is
that commit. Only the main branch's first-parent history counts, so a branch
commit mentioning the id is not a merge. One task, one such commit; a second
one is a mistake a tool reports (a reader uses the oldest). A `Task:` value
that is not a task id of the project is ignored by the derivation and
reported as a warning.

A revert of the merge is not undone by the derivation: the task stays
`merged`. Work that has to be redone is a new task.

## `tasks.toml`

```toml
format = 1

[project]
id_prefix   = "PRJ"     # ^[A-Z][A-Z0-9]*$; ids are "<id_prefix>-<number>"
main_branch = "main"    # the local branch whose first-parent history decides `merged`

[components.app]
paths   = ["src/**"]                              # non-empty
release = { rule = "tag", pattern = "v*" }        # or { rule = "merge" }

[routing]               # optional
default = "codex-default"
feature = "claude-main"
```

- `format` is `1`. Unknown keys anywhere are an error, so a misspelt key is
  rejected rather than ignored.
- A component's name matches `^[a-z][a-z0-9-]*$`; `release.rule` is `tag` or
  `merge`; `pattern` is present exactly when the rule is `tag`.
- `[routing]` says which agent profile a launcher runs a task of each kind
  with. Its keys are `default` and the kinds (`feature`, `fix`, `chore`,
  `research`, `design`); its values are profile names
  (`^[a-z][a-z0-9-]*$`) defined by the launcher's configuration. A launcher
  chooses, in order: a person's choice at launch → the task's `model` /
  `profile` → `routing.<kind>` → `routing.default` → its own default.
  Readers that launch nothing only validate the table.

`tasks.example.toml` beside this document is an annotated example.

## Releases and `done`

`tasks.toml` names each component's paths and its release rule:

- `rule = "tag"` with `pattern` — `done` when a tag matching the glob
  contains the task's merge commit (`git tag --contains <commit> --list
  <pattern>`);
- `rule = "merge"` — the component ships nothing separately (documents,
  plans); `done` as soon as it is `merged`.

A project with several deliverables — a backend, a frontend, a mobile app, a
CLI — gives each its own component and rule.

## Dependencies and parallel work

- `depends_on` names tasks that must be `merged` before this one starts: the
  new branch starts from a main branch that already holds them.
- The graph must have no cycle and name only existing ids.
- Two tasks may run at the same time only if neither depends on the other,
  directly or through others, **and their `touches` do not overlap**. Two
  patterns overlap when the literal prefix of one — the part before its first
  `*`, `?` or `[` — is a prefix of the other's literal prefix, path segment by
  path segment. A literal prefix that ends inside a segment (`src/ro*`)
  matches that segment by string prefix, so `src/ro*` overlaps
  `src/root/a.rs`; a pattern without a glob is its own literal prefix, so
  `src/ro` does not. A pattern starting with a glob overlaps everything. The
  rule is deliberately coarse: a false overlap costs a wait, a missed one
  costs a merge conflict between two agents.
- A task in `in_progress` or in `review` holds its `touches`: a task waiting
  for review is not merged yet, and work started beside it would conflict
  with it just the same.
- A file that every change of a kind appends one line to — a changelog — is
  not listed in `touches`: listing it would serialise all user-visible work
  for a conflict of one line that the merge resolves by keeping both.
- `after_release` holds a task until a release of its component contains the
  named commit.
- A task with `needs_person: true` is started only by a person, or with a
  person's explicit permission for that run.

## Taking and finishing a task

1. **Pick.** A task is takeable when its lifecycle is `backlog`, every
   `depends_on` is `merged` (or `done`), its `after_release` is satisfied, and
   no `in_progress` **or `review`** task's `touches` overlap its own — a task
   waiting for review still holds its paths until it is merged.
2. **Start.** The launcher creates a worktree and a branch from the current
   main branch — the branch name carries the id, e.g. `task/PRJ-007-<slug>` —
   writes the state file with `lifecycle: in_progress`, and launches the
   harness there with the task file as its assignment.
3. **Work.** The agent works to the Acceptance list, test first where there
   is code, and keeps the review limits in `reviews`. A disagreement with the
   task is written into the task file on the branch, not silently coded
   around. The harness status in the state file follows the program's OSC
   7501 reports.
4. **Hand over.** With every Acceptance box ticked and the Checks run, the
   launcher sets `lifecycle: review`. Ticking the boxes on the branch is part
   of the work: the merged file shows what was accepted.
5. **Merge.** The branch is merged into the main branch with `Task: <ID>` in
   the merge commit. The task is now `merged`, and `done` once a release of its
   component contains the merge.

A task that turns out too big for one merge is split before it is started:
the original is cancelled or narrowed, and the parts get new ids.

A `design` task's output is the decision recorded in OpenSpec (or the
project's place for decisions); it changes no production code. Tasks that
the decision changes are edited in the same branch.
