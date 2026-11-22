# taskmoor

A CLI with a TUI board for the tasks a repository keeps as files:
`tasks/<ID>-<slug>.md` (YAML front matter + Markdown body) and
`tasks/tasks.toml`. A person runs `taskmoor` in the project's terminal and
watches every task's lifecycle and the harness status of the running ones;
agents and launchers change the shared state through the CLI.

taskmoor is also the home of the format: [`spec/README.md`](spec/README.md),
`task.schema.json` and `state.schema.json`, `format = 1`. The schemas are
embedded in the binary.

## Install and build

Needs Go 1.26, git 2.45 or newer on `PATH` (taskmoor checks it: older git
fetches missing objects of a partial clone despite `GIT_NO_LAZY_FETCH`, and
lacks `worktree list -z` before 2.36) and
[`just`](https://github.com/casey/just) for the recipes.

```sh
just build          # bin/taskmoor
just check          # gofmt, go vet, golangci-lint (when installed), go test
```

Run the built binary by its path, or put it on `PATH`:

```sh
/path/to/taskmoor/bin/taskmoor --repo ~/work/some-project list
go install ./cmd/taskmoor    # into $(go env GOPATH)/bin
```

## Use

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

- The board, `list`, `show`, `ready` and `claim` read the tasks from the
  **main branch's tree**, so they look the same from every worktree.
  `validate` reads the **working tree** (what is about to be committed).
  `--source` overrides either.
- Exit codes: `0` ok; `1` validation errors or a refused transition; `2` a
  usage error or a project that cannot be read.
- Inside Guild (`GUILD_SOCKET` set, `guild` on `PATH`) the harness status
  comes from `guild agent list --json`; elsewhere from the state file.

### The board

Columns Backlog, In progress, Review, Merged, Done (`c` adds Cancelled).
`←→`/`hl` column, `↑↓`/`jk` task, `enter` detail, `/` filter, `r` ready
only, `R` refresh, `?` help, `q` quit. It polls every 2 s and reloads when
the main branch, the tags or the state files change.

### Lifecycle

| Lifecycle | From |
| --- | --- |
| `cancelled` | the task file on main says so |
| `done` | a release tag of the task's component contains its merge commit |
| `merged` | a first-parent commit on main carries `Task: <ID>` |
| `in_progress`, `review` | the shared state file `<git-common-dir>/tasks/state/<ID>.json` |
| `backlog` | otherwise |

`claim` writes the state file (atomic create-if-absent, under a per-project
`flock` on `<state>/.lock`), `review` rewrites it, `release` removes it (for
a stale file the lifecycle stays what git or the main branch says). The
three always read the main branch, whatever `--source` says. If the change
is published but syncing the state directory fails, the command still exits
0 and warns that the change may not survive a crash: other readers already
see it, so check the state (`taskmoor show ID`) rather than repeating it.

Readers never write: git runs with `GIT_NO_LAZY_FETCH=1` and
`GIT_OPTIONAL_LOCKS=0`, task files over 1 MiB (64 MiB together) and state
files over 64 KiB are refused, and only regular files are read.

## Layout

```
cmd/taskmoor/        entry point
spec/                the format (README, schemas, example tasks.toml), embedded
internal/gitx        runs git
internal/project     tasks.toml, task files, loading a snapshot
internal/schema      the embedded schemas, ECMA-262 regexps (regexp2)
internal/state       state files: read, atomic write, lock
internal/derive      lifecycle, readiness, touches overlap, cycles
internal/validate    every check as findings
internal/harness     Guild and state-file harness status
internal/app         the operations the CLI and the TUI share
internal/finding     the finding type
internal/text        control characters out of printed text
internal/cli         subcommands, output, exit codes
internal/tui         the Bubble Tea board
internal/testrepo    real git repositories for tests
skills/taskmoor/     how an agent uses taskmoor
docs/                the plan and the conveyor design
```
