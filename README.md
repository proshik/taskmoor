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

## The service (conveyor v1, stages 1–3)

`taskmoor serve` runs agent sessions on ready tasks: Claude (`claude -p`
stream-json on the person's subscription), Codex and OpenCode (over ACP),
driven from the CLI, the web board and the TUI. The dispatcher, review and
Telegram are the later stages in [`docs/conveyor.md`](docs/conveyor.md).

Needs the agent of each profile on the service's `PATH`: [Claude
Code](https://code.claude.com) signed in with the subscription (`claude`,
then `/login`); for Codex, `codex` signed in with ChatGPT (`codex login`)
and `npx` (the bridge `@agentclientprotocol/codex-acp@2.9.0` runs from
npm's cache; a profile's `command` can name an installed `codex-acp`);
for OpenCode, `opencode` signed in. A profile whose program is missing, or
whose sign-in does not fit its billing (Claude, Codex), is refused before
anything is created. OpenCode's sign-in is not checked up front: a
signed-out provider shows as a turn that does not move (OpenCode retries);
stop it and run `opencode auth login`.

```toml
[[profiles]]
name = "codex"
harness = "codex"          # ChatGPT sign-in; model = "…" picks one it offers

[[profiles]]
name = "opencode"
harness = "opencode"
api_billing = true         # required: opencode bills an API key here (A27)
model = "provider/model"
```

- **Routing.** A task's `profile` wins; otherwise `[routing]` by kind,
  `routing.default`, then `default_profile`. A task's `harness` (other
  than `any`) is a preference: the routed profile if it runs that harness,
  else the first configured profile that does. `taskmoor validate` warns
  when `profile` and `harness` disagree.
- **Restart with a profile.** `taskmoor restart ID --profile NAME` (or the
  card's "Restart with") stops the session and starts the chosen profile
  on the same claim, worktree and branch, told to continue from the work
  there. The old session stays in the history as stopped.
- **Keep N busy.** `taskmoor keep N` (or "Keep busy" in the board's top
  bar) has the service start ready tasks, priority then id, with their
  routed profiles, until N sessions of the project are under way
  (waiting for you counts); `needs_person` tasks are never started; a task
  whose start failed is skipped for 10 minutes and the board shows why.
  `taskmoor keep 0` turns it off; running sessions go on. A manual Run
  is never held back by it, so it can take the project one past N. An
  automatic start whose agent fails after the claim (no answer from the
  agent, a refused sign-in) leaves the task claimed with the session in
  error and its slot free: Restart it with a profile, or Release it.
- **The dispatcher (stage 4).** With `dispatcher_profile = "NAME"` (a
  claude profile; a cheap model is enough) in the config, an agent's
  question is first looked at by the dispatcher (C3):
  - (a) the project's documents answer it: it answers with a citation
    that must be found verbatim in the named file; documents are read
    from the main branch (`AGENTS.md`, `CLAUDE.md`, `README.md`,
    `docs/*.md`, `spec/*.md` and the task file), never the task's branch;
  - (b) technical, reversible, inside the task, with a recommended
    option: it picks the recommended option;
  - (c) everything else is yours, and always: push, merge, publish,
    deploy, deletions, secrets, dependencies, licences, migrations,
    billing, free-text and multi-choice questions, a reply that does not
    check out, a failed or API-billed call. Approvals are never the
    dispatcher's.

  Its call is `claude -p` with no tools, no settings but its own, in an
  empty directory, 2 minutes at most. The feed says "answered by the
  dispatcher"; `taskmoor decisions` (and the inbox) lists every decision
  with its reason and citation; `taskmoor override ID N` (or Override on
  the board) sends your answer to the same session as an instruction
  to change course (again if needed: each override is sent). Without
  `dispatcher_profile` every question is yours; with managed Claude
  settings that set hooks or an `apiKeyHelper`, every question is yours
  too (they would run in, or bill, the dispatcher's call). Bedrock, Vertex
  and Foundry switches are removed from every Claude process unless the
  profile allows API billing. An override is sent to the session like
  Say: mid-turn it waits for the next turn, so a session stopped before
  then does not get it; override again. Before going back to an older
  taskmoor, remove `dispatcher_profile` from the config (older versions
  refuse unknown keys).
- **Codex and OpenCode sessions.** Questions arrive as forms (Codex asks in
  its Default mode with `default_mode_request_user_input`); approvals as
  ACP permission requests, through the same rules as Claude's; what the
  rules cannot judge (more sandbox access, a network destination, a
  command outside the worktree, a fetch, a rename without its
  destination, another kind) always comes to you. Codex runs
  in its own workspace-write sandbox (writes in the worktree, no network,
  reads not confined; shown as "codex sandbox"). OpenCode has no sandbox
  ("unconfined"); its shell commands, edits outside the worktree and
  fetches are asked, its own and the repository's configuration are not
  loaded (MCP servers, plugins). Continue in terminal gives `codex resume
  ID` or `opencode --session ID`. Your Codex configuration (`~/.codex`,
  hooks, MCP servers, AGENTS.md) applies to Codex sessions.

```sh
taskmoor serve                          # in its own terminal tab: 127.0.0.1:7733; ctrl+c stops it
taskmoor project add ~/work/some-project
cd ~/work/some-project
taskmoor ready
taskmoor run PRJ-007                    # worktree ../some-project-prj-007, branch task/PRJ-007-<slug>, claim, start Claude
taskmoor feed PRJ-007 --follow          # what the agent does (a task id is looked up in this directory's project)
taskmoor inbox                          # its questions and approvals
taskmoor answer <ask> 1                 # pick an option (* marks the recommended one)
taskmoor say PRJ-007 "also update the README"
taskmoor stop PRJ-007                   # the claim, worktree and branch stay
taskmoor release PRJ-007                # through the service: stops the session, removes the state file
```

- **The web board (stage 2).** `taskmoor serve` prints a one-time sign-in
  link; `taskmoor web` opens a new one in the browser. Keep the link's
  `127.0.0.1` address (`localhost` is another origin with no sign-in).
  After installing a new taskmoor, restart a running `taskmoor serve`
  (ctrl+c in its tab, then start it again): the old process serves the
  old API. A service started in the background of a shell (`&`) is
  stopped with `fg` then ctrl+c, or `kill %1`. The board has the
  conveyor's columns (Backlog, Ready, Running, Needs you, Review, Merged,
  Done), a project switcher, a task card with Run (the routed profile
  preselected), the live feed, Say, Stop, Release, Open in Finder / the
  configured editor (`editor = ["code", "-n"]`), and Continue in terminal
  (`taskmoor continue ID` does the same from the CLI), and the inbox with
  forms. It follows the service's events and shows "Disconnected" while the
  service is unreachable. The TUI shows the same columns when the service
  runs and has the project registered.
- **Config** `~/.config/taskmoor/config.toml` (projects, profiles with
  `harness`, `model`, `api_billing`, `sandbox`, `allowed_domains`,
  `default_profile`, `port`); **store**
  `~/.local/state/taskmoor/` (session journals, the API token). XDG
  variables move them; `TASKMOOR_HOME` holds both.
- **The agent's rules.** Every command, edit and fetch comes to the service
  (ask rules beat the allow rules of your Claude settings), in Claude's
  default permission mode. Push, publish, deploy (also `kubectl`, `helm`,
  `terraform` changes), switching to the main branch and git writes in the
  main checkout are refused without asking (also as Claude deny rules).
  Reads of secret files wait in the inbox. File-tool
  writes outside the worktree, secret files, dependency manifests, and in
  commands, output redirections and copy/move/mkdir/… targets outside the
  worktree (temporary directories excepted) and deletions outside it wait in
  the inbox; everything else is allowed. The checks of a shell command are
  best effort: they read the command, not what it runs.
- **The sandbox (stage 2b).** A Claude profile runs the agent's commands in
  Claude's sandbox (Seatbelt on macOS; checked on macOS only) unless it
  sets `sandbox = false`. It needs Claude Code 2.1.330 or newer; an older
  one is refused at Run.
  - writes only to the worktree, the git objects, the worktree's own git
    directory and the task branch's ref, temporary directories and build
    caches; every other existing branch (`main` among them), the main
    checkout's `HEAD` and index, `hooks`, `config`, `info` and taskmoor's
    `tasks/` are read-only (a new branch can still be created);
  - no reads of taskmoor's store (the API token) and configuration, or of
    `~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.config/gh`, `~/.netrc`, `~/.npmrc`,
    `~/.docker`, `~/.kube`, `~/.git-credentials`, `~/.claude.json` and the
    keychains;
  - network only to the profile's `allowed_domains` (default: the Go, npm,
    PyPI and crates registries and GitHub for reading); other hosts are
    refused, not asked;
  - no way out of it (`allowUnsandboxedCommands = false`), and no session
    when the sandbox cannot start. Bash still asks the host for every
    command. The sandbox confines commands; Claude's own file tools are
    held by the host's rules above and by deny rules for taskmoor's files
    and the credentials (also in recursive Grep and Glob);
  - only taskmoor's Claude settings: your settings, the repository's
    `.claude/` settings, their hooks, plugins and MCP servers are not
    loaded (they would run or reach outside the sandbox). The project's
    `CLAUDE.md` and `.claude/rules/` may not load either: the assignment
    tells the agent to read them, and a task's own instructions belong in
    its file;
  - managed Claude settings that turn the sandbox off refuse a confined
    Run; a repository on git's reftable backend is refused too (the
    branch rules work on loose refs).

  Known limits: branches created after a session started are not
  protected from it; a rebase that changes tracked `.claude/` files fails
  inside the sandbox (run it yourself in the worktree); checked on macOS
  only.

  A sandbox refusal (an old Claude, managed settings, the ref backend)
  comes before anything is created. If a confined session fails after it
  started, the task stays claimed with its worktree and branch:
  `taskmoor release ID` frees the claim; keep or commit what the worktree
  holds, then `git worktree remove` it and delete the branch before a new
  Run. **Continue in terminal** hands the conversation to your own
  `claude`, with your settings and without taskmoor's sandbox or rules.

  A session with `sandbox = false`, or a harness without a sandbox, is
  shown as **unconfined** on the board and in `taskmoor sessions`: it runs
  as you, with your files, keychain and network. Release tokens in the
  environment (`NPM_TOKEN`, `GH_TOKEN`, …) are removed from every agent's
  environment.
- **Billing.** API-key variables are removed from the agent's environment,
  a Claude settings file with `apiKeyHelper` refuses the run, and a session
  whose `init` does not say `apiKeySource: none` is stopped, unless the
  profile sets `api_billing = true`.
- When the agent finishes, the task moves to `review`. While a service
  owns a task (a lock in the project's state directory), the CLI's `review`
  refuses it and `release` goes through that service.

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
internal/serve       taskmoor serve: config, journal, rules, the Claude runner, API, client
internal/testrepo    real git repositories for tests
skills/taskmoor/     how an agent uses taskmoor
docs/                the plan and the conveyor design
```
