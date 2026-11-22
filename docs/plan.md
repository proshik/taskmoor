# taskmoor — plan

Status: **agreed by the user as a whole (2026-11-22), with D1–D5 as
recorded; phase 0 implemented** together with the conveyor's A25 (`profile`,
`model`, `[routing]` in `spec/`, schema and validation only, `format = 1`)
and A29 (claims under `flock` on `<state>/.lock`). The decisions are
recorded at the end; what implementation settled is under "Implementation
decisions".

The agent conveyor design in [`conveyor.md`](conveyor.md) extends this
plan; this plan's MVP is the conveyor's foundation (phase 0).

## Goal

A CLI with a TUI that shows and drives the tasks a repository keeps as files
(`tasks/*.md` + `tasks/tasks.toml`). A person runs `taskmoor` in a project's
terminal tab and watches every task's lifecycle and the harness status of the
running ones; agents and launchers change the shared state through the CLI;
several tasks run in parallel.

taskmoor becomes the home of the format: the spec and both schemas live in
this repository, versioned as `format = 1`. Guild is the first user.

## MVP scope

In:

- The format spec and schemas under `spec/` (taken from Guild's `tasks/`,
  made tool-neutral), embedded into the binary.
- Reading a project: `tasks.toml`, task files, the shared state directory,
  git (first-parent trailers on the main branch, release tags).
- Status derivation, readiness, validation.
- State writes: `claim`, `review`, `release`.
- Harness status: from Guild (`guild agent list --json` when `GUILD_SOCKET`
  is set), otherwise from the state file's `harness` record, otherwise
  `unknown`.
- TUI board with detail view, filter and live refresh.
- `skills/taskmoor/SKILL.md`, `README.md`, `justfile`.

Out (deferred, see the end): launching a harness, merging, release cutting,
writing harness status into state files, pruning stale state.

## Repository layout

```
cmd/taskmoor/main.go         entry: parse args, run CLI or TUI
spec/
  README.md                  the format, format = 1 (moved from Guild's tasks/README.md)
  task.schema.json           front matter schema
  state.schema.json          shared state schema
  tasks.example.toml         an annotated tasks.toml
  spec.go                    go:embed of the two schemas
internal/gitx/               runs `git`; repo root, common dir, refs, trailers, tags, blobs
internal/project/            tasks.toml, task files (front matter + body), loading a project
internal/schema/             compiles the embedded schemas, validates values
internal/state/              state files: read, validate, atomic write (temp + rename / link)
internal/derive/             lifecycle, readiness, touches overlap, dependency graph
internal/validate/           all checks, as a list of findings
internal/harness/            Source interface; Guild source; state-file source
internal/cli/                subcommands, text and JSON output, exit codes
internal/tui/                Bubble Tea model: board, detail, filter, help
skills/taskmoor/SKILL.md     how an agent uses taskmoor
docs/plan.md                 this plan
README.md, justfile, .gitignore
```

`internal/derive` is pure: it takes a loaded snapshot (tasks, project, git
facts, state files, harness records) and returns statuses. Everything that
touches the disk or runs a program sits behind small interfaces so tests feed
fixtures. A future `launch` or `merge` command adds a package beside `state`
and a key in the TUI; nothing in `derive` changes.

## Dependencies

| Need | Module | Why |
| --- | --- | --- |
| TUI | `charm.land/bubbletea/v2` v2.1, `charm.land/bubbles/v2` v2.2, `charm.land/lipgloss/v2` v2.0 | The current Charm major line. |
| Markdown | `charm.land/glamour/v2` v2.0 | Renders the task body; same family. |
| YAML | `go.yaml.in/yaml/v3` | The YAML org's maintained continuation of `gopkg.in/yaml.v3` (archived). Used at the `yaml.Node` level, see below. |
| TOML | `github.com/BurntSushi/toml` v1.6 | Mainstream; strict decoding with undecoded-key reporting. |
| JSON Schema | `github.com/santhosh-tekuri/jsonschema/v6` | Maintained, full draft 2020-12, pluggable regexp engine. |
| ECMA regexp | `github.com/dlclark/regexp2` | The validator's regexp engine (D3): the schema's `touches` pattern uses lookahead, which Go's RE2 `regexp` rejects. Pure Go, no dependencies. |

CLI parsing uses the standard library `flag` package per subcommand; no
framework. Git is read by running `git` (`os/exec`): the format is defined in
git's own terms (`git interpret-trailers`, `git tag --contains`,
`--git-common-dir`), running git gives exactly those semantics, and go-git
would add a large dependency that differs from git on edge cases (worktrees,
packed refs). No fsnotify (see Live refresh).

## How a project is found

- The repository is `git rev-parse --show-toplevel` from the working
  directory or `--repo PATH`. The task directory is `tasks/` at its root
  (fixed in format 1).
- The common dir is `git rev-parse --git-common-dir`; the state directory is
  `<common>/tasks/state/`.
- **Task source (D1).** The board, `list`, `show`, `ready` and `claim` read
  `tasks/` from the **main branch's tree** (`git ls-tree` + one
  `git cat-file --batch`), because the format defines a task's `status` as
  what the file says on the main branch and the board must look the same from
  every worktree. `validate` reads the **working tree** by default, because it
  checks what is about to be committed. `--source main|worktree` overrides
  either. `tasks.toml` is read from the same source as the tasks.
- The main branch is `tasks.toml`'s `project.main_branch`, read as the local
  branch `refs/heads/<main_branch>`.

## Reading a task file

- File name `<ID>-<slug>.md`, slug `^[a-z0-9]+(-[a-z0-9]+)*$`.
- The front matter is the text between a first line `---` and the next line
  `---`; the rest is the body.
- YAML is decoded to a `yaml.Node` and converted to JSON values by hand:
  `!!str` → string, `!!int` → integer, `!!bool`, `!!float`, `!!null`, maps and
  sequences. An unquoted date (`!!timestamp`) is an error that says to quote
  it; an all-digit commit is `!!int` and fails the schema's `string`. Anchors
  and merge keys are refused. Duplicate keys are an error.
- The JSON value is validated against the embedded `task.schema.json`, then
  decoded into a Go struct.

## How status is derived

Per the spec, first match wins:

1. file `status: cancelled` (main branch) → `cancelled`;
2. a release of the component contains the task's merge commit → `done`;
3. a commit on the main branch's first-parent history carries `Task: <ID>` →
   `merged`;
4. a valid state file says `in_progress` or `review` → that;
5. otherwise → `backlog`.

Git facts, gathered once per refresh:

- **Merges.** One call:
  `git log --first-parent refs/heads/<main> --format=%H%x1f%(trailers:key=Task,valueonly,separator=%x1e)%x00`.
  `%(trailers)` is git's own trailer parser, so "the last paragraph" means
  what `git interpret-trailers` means. Values that do not match the id pattern
  are ignored by derivation and reported by `validate` as warnings (Guild's
  main already has `Task: osc7501-agent-status` on `0068f472`). A second
  commit for one id is a `validate` error; derivation uses the oldest one.
- **Releases.** For a `rule = "tag"` component:
  `git tag --list <pattern> --contains <merge>`; the first tag by version sort
  is shown as the release. Cached by `(commit, pattern)` until the tag set
  changes. `rule = "merge"` → `done` as soon as `merged`.
- **`after_release`.** Satisfied when a tag of the component's pattern
  contains the commit; for `rule = "merge"`, when the commit is an ancestor of
  the main branch.

State files: a missing, unreadable or schema-invalid file is "no state"; the
problem is reported (a warning in `list`/board, an error in `validate`), never
guessed around. A state file for a task git already shows `merged`/`done`, or
`cancelled`, is **stale**: shown as such, not deleted (readers do not write).

## Readiness (`ready`)

A task is ready when all hold:

- lifecycle `backlog`;
- every `depends_on` is `merged` or `done`;
- `after_release` (if any) is satisfied;
- its `touches` overlap no task in `in_progress` **or `review` (D4)**.

Overlap per the spec: the literal prefix of a pattern is the part before its
first `*`, `?` or `[`; two patterns overlap when one literal prefix is a
prefix of the other, compared path segment by path segment (a partial last
segment of a prefix matches by string prefix: `src/ro*` overlaps
`src/root/a.rs`).

`needs_person: true` does not make a task unready; it is marked everywhere
(`⚑` on the board, `needs_person: true` in JSON) and `claim` asks for
`--person`. Each unready backlog task carries its reasons
(`dependency GLD-006 is backlog`, `touches overlap GLD-011 (in_progress):
crates/session/**`, `after_release 0068f472 not in a nightly-* tag`).

## Harness status

`internal/harness.Source` returns records keyed by task id:
`{name, status, kind?, exit_code?, observed_at, source}` with `status` in the
format's seven words and `source` = `guild` | `state` .

- **Inside Guild** (`GUILD_SOCKET` set and `guild` on `PATH`): run
  `guild agent list --json` with a 2 s timeout. Guild's wire words differ from
  the format's and are mapped:

  | Guild | format |
  | --- | --- |
  | `exited: true` | `exited` |
  | `idle`, `working`, `done`, `unknown` | same |
  | `waiting` (+ `kind`) | `blocked` (+ `kind` only if Guild sent one) |
  | `failed` | `error` |
  | any other word | `unknown` (the raw word kept in JSON as `raw`) |

  An agent belongs to a task when its `session` equals the state file's
  `harness.session` (**D2**); without one, when its `worktree` equals the
  state file's `worktree` (both cleaned and symlink-resolved). Several agents
  in one worktree: prefer `task: true` and `open: true`; still several →
  `unknown` with "several sessions in the worktree". A failed or timed-out
  call → fall back to the state source, and the board's footer says
  "Guild: unavailable".
- **Elsewhere**, or for a task Guild knows nothing about: the state file's
  `harness` record if present (`source: state`).
- An `in_progress`/`review` task with neither → `unknown`. Tasks without
  state show no badge.

taskmoor **does not write** observed harness status into state files: the
spec gives that to whoever runs the harness (the launcher / Guild). The only
harness record taskmoor writes is the initial one at `claim --harness`.
`taskmoor serve` (the conveyor, [`conveyor.md`](conveyor.md), A1) is a
launcher and keeps the harness record current for the sessions it runs.

## State writes

All three commands load the project (main-branch source), derive, refuse
when the transition is not allowed, build the new file, validate it against
the embedded `state.schema.json`, and write it atomically:

- the file is written to `<state>/.<ID>.<random>.tmp` in the same directory,
  `fsync`ed, then
- **claim** publishes it with `link(tmp, <ID>.json)` — atomic create-if-absent,
  so two concurrent claims cannot both win — and removes the temp file;
- **review** publishes it with `rename(tmp, <ID>.json)`;
- the directory is `fsync`ed after publishing.

`writer` is `taskmoor <version>`. Times are UTC RFC 3339 with seconds and `Z`.

`taskmoor claim ID --branch B --worktree W [--harness H] [--session S] [--person] [--force]`

- refuses unless the task is `backlog` and ready (reasons printed);
  `--force` skips the readiness check only, never the lifecycle check;
- refuses a `needs_person` task without `--person` (the caller states a
  person started it or permitted this run);
- `W` must be an absolute path listed by `git worktree list --porcelain` with
  branch `B` checked out; `base` = `git merge-base B refs/heads/<main>`;
- `--harness H` adds `harness {name: H, status: unknown, observed_at: now}`
  (and `session: S` with D2); `--session` without `--harness` is a usage error;
- refuses if a state file exists (another writer owns the task).
- the readiness re-check and the publish run under a per-project lock,
  `flock` on `<state>/.lock`, shared with `taskmoor serve` (conveyor A29),
  so two claims of different ids with overlapping `touches` cannot both win.

`taskmoor review ID` — state must exist with `in_progress` (or already
`review`: no-op, exit 0); keeps everything, sets `lifecycle: review`,
`updated_at`, `writer`.

`taskmoor release ID` — back to backlog: removes the state file; refuses
when there is none. Allowed from `in_progress` and `review`, and for a stale
file.

## CLI surface

```
taskmoor [--repo PATH] [--source main|worktree]      the TUI board
taskmoor list     [--json]                           every task with its status
taskmoor show ID  [--json]                           front matter, status, reasons, state, body
taskmoor ready    [--json]                           takeable tasks, priority then id
taskmoor validate [--json]                           all checks; exit 1 on any error
taskmoor claim ID --branch B --worktree W [--harness H] [--session S] [--person] [--force]
taskmoor review ID
taskmoor release ID
taskmoor version
```

Global flags `--repo` and `--source` work before or after the subcommand.
Exit codes: `0` ok; `1` problems found (`validate` errors) or a refused
transition; `2` usage error or the project cannot be read. JSON output is one
object with `"format": 1`; errors go to stderr as text (and as
`{"error": …}` on stdout with `--json`).

`list --json` per task: `id, title, kind, priority, component, file,
lifecycle, needs_person, depends_on, touches, ready, reasons[],
merge_commit?, release_tag?, state? {…, stale}, harness? {…, source}`; plus
top-level `warnings[]` and `harness_source` (`guild` / `none` /
`guild-unavailable`).

## Validation (`validate`)

Errors (exit 1):

- `tasks.toml`: `format = 1`; `project.id_prefix` `^[A-Z][A-Z0-9]*$`;
  `project.main_branch` set and the branch exists; each component's name
  `^[a-z][a-z0-9-]*$`, non-empty `paths`, `release.rule` `tag`|`merge`,
  `pattern` present exactly for `tag`; unknown keys.
- Each task file: file-name shape; front matter present and parseable
  (unquoted date, duplicate key); `task.schema.json`; `id` equals the file
  name prefix; `id` uses `id_prefix`; ids unique; `component` exists;
  `depends_on` ids exist, not itself, no cycle (the cycle is printed);
  `after_release` names a commit on the main branch.
- Each state file: parseable, `state.schema.json`, file name equals `id`,
  the id is a known task.
- Git: a second `Task: <ID>` commit on the main branch's first-parent
  history.

Warnings (exit 0): stale state files; `Task:` trailers whose value is not a
project id or names an unknown task; a task file not on the main branch yet
(worktree source only).

Output: `path:line: message` where a line is known (YAML node lines), one per
finding, then a count.

## TUI

Bubble Tea v2 model; layout with Lip Gloss.

**Board** (default screen):

```
taskmoor · guild · main 3f2a1c9 · 52 tasks · Guild ●             filter: session
 Backlog 41          In progress 2       Review 1        Merged 0      Done 0
 ┌───────────────┐   ┌───────────────┐
 │GLD-007  P2 ✓  │   │GLD-011  P1    │
 │Heavy features │   │Session server │
 │off by default │   │● working      │
 └───────────────┘   └───────────────┘
 …
 ←→ column  ↑↓ task  enter open  / filter  r ready only  c cancelled  ? help  q quit
```

- Columns: Backlog, In progress, Review, Merged, Done; Cancelled hidden,
  shown as a sixth column with `c`. Each column sorted by priority then id
  and scrolls on its own; the header shows its count.
- Card: id, priority `P1`–`P4`, title (wrapped to two lines), and:
  backlog — `✓` ready or `·` not ready; `⚑` needs a person; running —
  harness badge coloured by status (`working` blue, `blocked` + kind yellow,
  `error` red, `done`/`idle` green, `unknown`/`exited` grey); `stale` mark on
  stale state.
- Narrow terminal (fewer than 5 × 24 columns): one column at a time with the
  column names as tabs; `←`/`→` switch.

**Detail** (`enter`): header with id, title, lifecycle, priority, kind,
component, harness; facts block (depends_on with their statuses, readiness
reasons, touches, branch/worktree/base/started from state, merge commit and
release tag); then the body rendered by glamour in a scrolling viewport
(width-aware, re-rendered on resize). `esc`/`q` back.

**Keys**

| Key | Board | Detail |
| --- | --- | --- |
| `←`/`h`, `→`/`l` | column | — |
| `↑`/`k`, `↓`/`j` | task | scroll |
| `pgup`/`pgdown`, `g`/`G` | page / first / last | page / top / bottom |
| `enter` | open detail | — |
| `esc` | clear filter | back |
| `/` | filter: substring over id, title, component (live, `enter` keeps, `esc` clears) | — |
| `r` | toggle: backlog shows ready only | — |
| `c` | toggle cancelled column | — |
| `R` | refresh now | refresh now |
| `?` | help overlay | help overlay |
| `q`, `ctrl+c` | quit | back (`ctrl+c` quits) |

The selection follows the task id across refreshes (a task that moves to
another column stays selected).

## Live refresh

Polling, not fsnotify. Every 2 s a tea command computes a cheap fingerprint:

- `git for-each-ref --format='%(objectname) %(refname)' refs/heads/<main> refs/tags`
  (one call; covers new merges and new release tags);
- names, sizes and mtimes of `<state>/*.json`;
- with `--source worktree`, names, sizes and mtimes of `tasks/*.md` and
  `tasks/tasks.toml`.

Only a changed fingerprint reloads the snapshot (in a goroutine, the result
arrives as a message). Guild's `agent list` is polled on the same tick when
`GUILD_SOCKET` is set (it is a socket query; Guild sends no events).

Why not fsnotify: git updates refs by lockfile + rename in nested directories
and by rewriting `packed-refs`, so a watcher needs one watch per directory
under `refs/` (fsnotify is not recursive; kqueue holds a descriptor per
watched path on macOS) and still misses nothing only after re-adding watches
as directories appear; and the Guild source needs a tick anyway. One loop is
simpler, testable with an injected clock, and a 2 s delay is fine for a
board. fsnotify can be added later behind the same "snapshot changed"
message.

## Tests

Test first for behaviour; fixtures are real repositories made by `git` in
`t.TempDir()`:

- `internal/testrepo`: helper to write `tasks.toml` and task files, commit,
  branch, merge with `--no-ff` and a `Task:` trailer, squash, tag, add a
  worktree, write state files.
- derive: each lifecycle rule and their order; trailer only on a branch (not
  first-parent) is not merged; trailer not in the last paragraph; second
  trailer; tag rule vs merge rule; `after_release`; stale state; invalid
  state file treated as none.
- readiness: dependencies, after_release, overlap cases from the spec
  (prefix by segment, globs, `review` counts per D4).
- validate: one fixture per error and warning; exit codes.
- state writes: claim/review/release transitions and refusals; concurrent
  claims (two goroutines, one wins); temp file never left behind; written
  file passes the schema; worktree/branch checks.
- harness: Guild word mapping and matching with a fake runner returning
  recorded JSON; timeout → fallback.
- YAML: unquoted date, all-digit commit, duplicate key, anchors.
- TUI: model driven through `Update` with key and window-size messages,
  assertions on `View()` (columns, selection, filter, detail, narrow layout,
  selection kept across a refresh message). No real terminal.
- CLI: `list --json`, `ready --json`, `validate` output and exit codes
  against a fixture.

Checks: `gofmt -l .` empty, `go vet ./...`, `go test ./...`;
`golangci-lint` is **not installed** on this machine, so `just check` runs it
only when present and says so otherwise. `justfile`: `check`, `test`,
`build` (to `bin/taskmoor`), `fmt`.

Real check: the built binary, read-only, in Guild's repository: `list`,
`validate`, `ready`, the TUI for a moment. No state files written there;
writes are checked on a fixture repository.

## Skill

`skills/taskmoor/SKILL.md`, front matter `name: taskmoor` and a
`description`; imperative and short: find (`ready`, `list`), read (`show`),
check (`validate`), what an agent may write (the task file on its branch —
ticked boxes, recorded disagreements; lifecycle through `claim`/`review`/
`release` when it is the launcher or asked to) and may not (its own harness
status, the state file by hand, `status` other than by a person's decision),
the `Task: <ID>` trailer on the merge commit, `needs_person`. Not installed
anywhere.

## Changes to the format

The spec moves to `spec/README.md` here; wording that names Guild is made
generic, taskmoor is named as a reference reader. Changes, as decided by the
user:

- **D2** — an optional `harness.session` (string) in `state.schema.json`, so
  a reader can find the running program for a task without guessing from the
  worktree. Additive; no reader or writer exists yet, so it stays format 1.
- **D3** — the `touches` item pattern uses lookahead
  (`^(?!/)(?!\./)(?!.*(^|/)\.\.(/|$))[^\s]+$`). It is valid ECMA-262 (the
  dialect JSON Schema names), but RE2-based engines — Go's `regexp`, Rust's
  `regex` — reject it.
- **D4** — readiness counts `review` tasks as running for `touches` overlap
  (the spec's "Pick" step names only `in_progress`).
- One clarification, no behaviour change: trailer values that are not a
  project id are ignored by derivation and reported as warnings.

After this lands, Guild's `tasks/README.md` should shrink to: "These tasks
follow the taskmoor task format, `format = 1`: `<taskmoor>/spec/README.md`"
plus Guild's own notes (its `tasks.toml`, components), and its copies of the
schemas either go or stay as pinned copies with a note that taskmoor's are
authoritative. That edit is Guild's, not this repository's.

## Deferred

- Launching a harness from the TUI (inside Guild:
  `guild worktree create task/<ID>-<slug> --from-file <task> --harness H`,
  then `claim`) — a `launch` package and a key; designed for, not built.
  Decided by the conveyor: [`conveyor.md`](conveyor.md).
- `merge` (adding the `Task:` trailer; decided by the conveyor:
  [`conveyor.md`](conveyor.md)), release cutting, pruning stale state.
- Carrying OSC 7501 / Guild status into the state file (the launcher's job).
- Reading a remote main (`origin/main`); several repositories in one board
  (decided by the conveyor: [`conveyor.md`](conveyor.md)).
- Creating or editing task files from the TUI; a `new` scaffold.
- fsnotify-based refresh.

## Decisions (answered by the user, 2026-11-07)

- **D1 — task source:** the main branch's tree for the board and
  `list/show/ready/claim`; the working tree for `validate`; `--source
  main|worktree` overrides.
- **D2 — `harness.session`:** added to `state.schema.json` as an optional
  string; matching by session first, by worktree otherwise.
- **D3 — `touches` pattern:** the schema keeps its lookahead pattern;
  taskmoor validates with an ECMA-262 engine (`github.com/dlclark/regexp2`).
- **D4 — overlap:** tasks in `in_progress` and in `review` both hold their
  `touches`; `spec/README.md` says so.
- **D5 — alias:** none; the binary is `taskmoor` only.

## Implementation decisions (phase 0, 2026-11-22)

Settled while implementing, without changing D1–D5:

1. **Module and versions.** `github.com/proshik/taskmoor`, Go 1.26. Direct
   dependencies as planned, at their current releases: bubbletea v2.1.2,
   bubbles v2.2.2, lipgloss v2.1.1 (the plan named v2.0; v2.1 is the same
   major line), glamour v2.0.1, `go.yaml.in/yaml/v3` v3.0.6, BurntSushi/toml
   v1.6.0, jsonschema v6.0.3, regexp2 v1.12.0. Two more are used directly
   but were already in the graph: `golang.org/x/text` (jsonschema's message
   printer) and `charmbracelet/x/ansi` (cell widths and truncation, from
   lipgloss).
2. **Two small packages beyond the layout.** `internal/app` holds the
   operations the CLI and the TUI share (load + derive + validate + harness
   into one report; `claim`/`review`/`release`); `internal/finding` is the
   finding type. `derive` stays pure: the loader gathers every git fact into
   the snapshot first.
3. **Finding the main branch.** Its name comes from `tasks.toml`, which the
   main source reads from the main branch. The name is learnt from the
   working tree's `tasks/tasks.toml` (HEAD's when the working tree has
   none); then the main branch's `tasks.toml` is the one used. The main
   source refuses (exit 2) when the branch does not exist; `validate` (worktree
   source) reports it as an error and skips the git checks.
4. **Which files are tasks.** Every `*.md` directly in `tasks/` except
   `README.md`; other files and subdirectories are ignored. `spec/README.md`
   says so.
5. **A duplicate id** across two files: the first file by name is used, the
   second is an error and is skipped.
6. **`claim` paths.** `--branch` may be a short name or `refs/heads/…`; the
   state file stores the short name. `--worktree` is stored cleaned as
   given; it is compared with `git worktree list` after resolving symlinks.
7. **The lock covers every write.** `claim`, `review` and `release` all
   load, re-check and publish under `flock` on `<state>/.lock`; the state
   directory and the lock file are created on the first write.
8. **`release`** removes any existing state file, also an invalid one (the
   way to clear it), for a known task or an id-shaped name; `review` refuses
   an invalid or stale state file.
9. **`list` warnings.** `warnings[]` carries every finding `validate`
   would report for the same source (errors included), as text; `list`'s
   text output prints them to stderr.
10. **Finding lines.** `path:line: severity: message`; the line is left out
    when unknown, the path when there is none (git findings). State-file
    paths are absolute (they live in the git common dir). `tasks.toml`
    lines are found by key, best effort.
11. **Harness records in JSON** add `session`, `raw` (Guild's unknown word)
    and `note` ("several sessions in the worktree", "no harness reported").
    A running task with neither a Guild match nor a state record gets
    `{status: unknown, source: none}`. Several agents in one worktree are
    narrowed by `task: true`, then by `open: true`.
12. **One refresh loop.** Inside Guild the fingerprint also includes the
    agent list, so a harness change reloads on the same tick; any change
    reloads the whole snapshot. Release lookups are cached per
    `(commit, pattern)` while the tag set is unchanged.
13. **TUI start and errors.** The first load runs before the screen opens;
    a project that cannot be read exits with 2. A failed reload keeps the
    last board and shows the error in the footer. SIGTERM ends the board
    quietly with exit 0.
14. **Cards** are six rows; the last row is the harness badge (running), a
    `stale` mark, the release tag or merge commit (finished), or the first
    unready reason (backlog). The selected card of the focused column has a
    thick border, of other columns a rounded one. Glamour's style follows
    the terminal background (dark by default).
15. **A25 scope.** `profile`/`model` are in `task.schema.json`; `[routing]`
    keys are `default` and the kinds, values match the profile-name
    pattern. The A16 warning (profile versus harness) is not in phase 0:
    without a profile configuration there is nothing to compare.
16. **Versions in output.** `-ldflags "-X main.version=…"`, else the
    module version, else `dev`; state files say `writer: "taskmoor
    <version>"`.
17. `show` of an unknown id exits with 2 (nothing to show, not a refused
    transition).
18. **Guild's trailer example.** `0068f472` (`Task: osc7501-agent-status`)
    is not on Guild main's first-parent history any more, so the real check
    shows no trailer warning; the warning is covered by a fixture test.
19. **Spec text.** `spec/README.md` is Guild's README made generic (`PRJ`
    examples), plus a `tasks.toml` section with `[routing]`, the
    partial-segment overlap rule, D2's `harness.session`, D4, publish by
    `link`/`rename` and the lock (A29); "a tool may delete a stale state
    file" became "readers never delete it; `release` does".
20. **Writes read the main branch.** `claim`, `review` and `release` load
    the main source under the lock whatever `--source` says, so a local edit
    cannot drop `needs_person` or a dependency; `--source` only changes what
    the read commands show. A valid running state file of a task that is
    not on the main branch is a readiness reason for every backlog task
    (its touches are unknown), so `ready`, the board and `claim` agree;
    `--force` skips it like any readiness reason.
21. **`claim` checks the worktree itself**, not only git's record: a
    `prunable` entry, a missing directory or a different checked-out branch
    is refused. `base` is the merge base with the resolved main commit.
22. **One main commit per load.** The main branch is resolved once to an
    exact local branch (`show-ref --verify refs/heads/<name>`; `main~1` is
    not a branch) and that commit is used for the tree, the trailers, the
    ancestry checks and the merge base.
23. **Readers do not write and stay bounded.** git runs with
    `GIT_NO_LAZY_FETCH=1` (a partial clone is not fetched; a missing object
    is reported) and `GIT_OPTIONAL_LOCKS=0`. `cat-file --batch` is streamed;
    a task file or `tasks.toml` over 1 MiB, task files over 64 MiB together
    and a state file over 64 KiB are refused. Only regular files are read,
    opened without following symlinks or blocking on a FIFO; anything else
    named `<ID>.json` is reported and `release` removes it. The tag pattern
    is passed after `--`, so it is never read as an option. Paths from git
    keep trailing spaces.
24. **Numbers in state files** are decoded from the validated value, so
    `1.0` for an integer is accepted as the schema accepts it.
25. **A change published without a durable directory sync** is reported as
    made: exit 0 with a warning that it may not survive a crash (the README
    says to check the state rather than retry).
26. **Waiting for the lock** polls a non-blocking `flock` and ends when the
    command is cancelled (SIGTERM).
27. **`validate` with an unusable `tasks.toml`** (syntax or type error, no
    `main_branch`) reports the findings with exit 1 and checks only what
    needs no configuration; the main source still refuses with exit 2.
    Keys are checked by their exact spelling (the decoder matches fields
    case-insensitively). Anchors on keys are refused like anchors on values.
28. **`release`** says what the task is afterwards ("back to backlog" or
    "stale state file removed; still merged").
29. **Terminal safety.** Text from task files and state files is printed
    with control characters replaced by U+FFFD (a body keeps its newlines
    and tabs), in the CLI and the board.
30. **Refresh compares with what the report read.** A load takes the
    fingerprint (refs, state and task file listings) before reading
    anything, and the Guild part from the very agent list it used; the
    board compares each tick's fingerprint with the shown report's, so a
    change during a load, or a change and its undo between two ticks, still
    reloads. One poll runs at a time. The header keeps `⚠ N problems (?)`
    visible at any width; `?` lists them with the exact `taskmoor --repo …
    validate --source …` command. Narrow tabs scroll to keep the active one
    visible; a resize keeps the selected card in view.
31. `--` ends a subcommand's flags; a global flag error with `--json`
    prints the JSON error object.
32. **git 2.45 or newer**, checked once per run: older git fetches missing
    objects of a partial clone despite `GIT_NO_LAZY_FETCH`; `worktree list
    -z` needs 2.36. Variables that point git elsewhere (`GIT_DIR`,
    `GIT_WORK_TREE`, `GIT_INDEX_FILE`, …) are dropped for every git child,
    so `--repo` (or the working directory) alone selects the repository.
    Only the final LF of a path git prints is removed.
33. **`Task:` trailers** are read unfolded, one per line, so a value is
    always checked whole against the id pattern.
34. **The main branch cannot be redirected locally.** With the main source,
    the main branch's own `tasks.toml` must name the branch the working
    tree's `tasks.toml` chose, else the load fails.
35. **`claim` refuses when `tasks.toml` on main has errors** (even with
    `--force`): statuses depend on its components and release rules.
    `claim` also refuses a checkout at a registered worktree path that
    belongs to another repository (its git common dir differs), and a branch
    that does not contain the merge commit of a dependency (without
    `--force`).
36. **Writers resolve the repository once** (top level, symlinks resolved)
    before taking the lock, load that path, and refuse when its state
    directory is not the locked one.
37. **Bounds.** A task with more than 1000 `touches` entries is refused (the
    overlap check is pairwise and runs under the lock); literal prefixes
    are split once per load; empty segments (`src//a.go`) are dropped. A
    state file that would exceed 64 KiB is not written. A directory sync
    error, `EINVAL` included, is reported as a change that may not be
    durable.
38. **Symlinks.** A symlinked `tasks.toml` or task file is not read: in the
    working tree it is a finding, on the main branch a `tasks.toml` that is
    not a regular file fails the load and such a task file is a finding.

