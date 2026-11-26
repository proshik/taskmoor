# Using the service

`taskmoor serve` runs agent sessions on ready tasks: Claude (`claude -p`
stream-json on the person's subscription), Codex and OpenCode (over ACP),
driven from the CLI, the web board and the TUI. This page is the
reference for using it; the design, and the stages that added each part
with every decision taken, are in [`conveyor.md`](conveyor.md). The
[README](../README.md) has the install and the quick start. This page
was split out of the README: where a decision in `conveyor.md` says the
README describes a part of the service, it is described here.

- [What runs where](#what-runs-where)
- [Agents and sign-ins](#agents-and-sign-ins)
- [Configuration](#configuration)
- [Running a task](#running-a-task)
- [The web board](#the-web-board-stage-2)
- [Profiles and routing](#profiles-and-routing): routing, restart with a
  profile, keep N busy
- [The dispatcher](#the-dispatcher-stage-4) and
  [the dispatcher's profile](#the-dispatchers-profile-stage-13)
- [Review and acceptance](#review-and-acceptance-stage-5)
- [Codex and OpenCode sessions](#codex-and-opencode-sessions)
- [The agent's rules](#the-agents-rules),
  [Claude's sandbox](#claudes-sandbox-stage-2b) and
  [billing](#billing)
- [Usage and power](#usage-and-power-stage-6)
- [Telegram](#telegram-stage-7)
- [Ideas and intake](#ideas-and-intake-stage-8-v2)
- [The web from outside](#the-web-from-outside-stage-9-v2) and
  [the board as a phone app](#the-board-as-a-phone-app-stage-10)
- [Cleaning up](#cleaning-up-stage-12)

## What runs where

The service and the agents run on your Mac, under your sign-ins: nothing
of taskmoor runs anywhere else. Reaching the board from a phone outside
home is optional: a VPS of yours runs stock images (Caddy, Pocket ID,
oauth2-proxy, from the `docker-compose.yml` that `taskmoor remote files`
writes) and the Mac keeps a reverse ssh tunnel to it. There is no taskmoor
image to deploy.

## Agents and sign-ins

The service needs the agent of each profile on its `PATH`: [Claude
Code](https://code.claude.com) signed in with the subscription (`claude`,
then `/login`); for Codex, `codex` signed in with ChatGPT (`codex login`)
and `npx` (the bridge `@agentclientprotocol/codex-acp@2.9.0` runs from
npm's cache; a profile's `command` can name an installed `codex-acp`);
for OpenCode, `opencode` signed in. A profile whose program is missing, or
whose sign-in does not fit its billing (Claude, Codex), is refused before
anything is created. OpenCode's sign-in is not checked up front: a
signed-out provider shows as a turn that does not move (OpenCode retries);
stop it and run `opencode auth login`.

## Configuration

`~/.config/taskmoor/config.toml` names the agent profiles (the agent and
its model) and, optionally, the dispatcher:

```toml
default_profile = "claude"
dispatcher_profile = "dispatcher"   # optional: answers questions the docs answer

[[profiles]]
name = "claude"
harness = "claude"
description = "careful changes across the code, with tests"

[[profiles]]
name = "claude-haiku"
harness = "claude"
model = "haiku"
description = "typos, docs edits, small well-specified changes"

[[profiles]]
name = "dispatcher"
harness = "claude"
model = "sonnet"

[[profiles]]
name = "codex"
harness = "codex"          # ChatGPT sign-in; model = "…" picks one it offers
description = "larger changes across the code"  # optional: what the dispatcher reads

[[profiles]]
name = "opencode"
harness = "opencode"
api_billing = true         # required: opencode bills an API key here (A27)
model = "provider/model"
```

**Config** `~/.config/taskmoor/config.toml` (projects, profiles with
`harness`, `model`, `api_billing`, `sandbox`, `allowed_domains`,
`default_profile`, `port`); **store** `~/.local/state/taskmoor/` (session
journals, the API token). XDG variables move them; `TASKMOOR_HOME` holds
both.
## Running a task

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

When the agent finishes, the task moves to `review`. While a service owns
a task (a lock in the project's state directory), the CLI's `review`
refuses it and `release` goes through that service.

## The web board (stage 2)

`taskmoor serve` prints a one-time sign-in link; `taskmoor web` opens a
new one in the browser. Keep the link's `127.0.0.1` address (`localhost`
is another origin with no sign-in). After installing a new taskmoor,
restart a running `taskmoor serve` (ctrl+c in its tab, then start it
again): the old process serves the old API. A service started in the
background of a shell (`&`) is stopped with `fg` then ctrl+c, or
`kill %1`. The board has the conveyor's columns (Backlog, Ready, Running,
Needs you, Review, Merged, Done), a project switcher, a task card with Run
(the routed profile preselected), the live feed, Say, Stop, Release, Open
in Finder / the configured editor (`editor = ["code", "-n"]`), and
Continue in terminal (`taskmoor continue ID` does the same from the CLI),
and the inbox with forms. It follows the service's events and shows
"Offline" while the service is unreachable. The TUI shows the same columns
when the service runs and has the project registered. **Guide** in the top
bar opens a getting-started checklist: each step of a task's way through
the conveyor ticks itself from what the board shows, and **Show me** opens
the place where it happens; it opens by itself the first time a browser
shows the board. The board follows the system's light or dark setting; its
fonts (Red Hat Text and Mono, OFL) are served by taskmoor itself, so it
works offline and loads nothing from elsewhere.

## Profiles and routing

### Routing

A task's `profile` wins; otherwise `[routing]` by kind, `routing.default`,
then `default_profile`. A task's `harness` (other than `any`) is a
preference: the routed profile if it runs that harness, else the first
configured profile that does. `taskmoor validate` warns when `profile` and
`harness` disagree.

### Restart with a profile

`taskmoor restart ID --profile NAME` (or the card's "Restart with") stops
the session and starts the chosen profile on the same claim, worktree and
branch, told to continue from the work there. The old session stays in the
history as stopped.

### Keep N busy

`taskmoor keep N` (or "Keep busy" in the board's top bar) has the service
start ready tasks, priority then id, with their routed profiles, until N
sessions of the project are under way (waiting for you counts);
`needs_person` tasks are never started; a task whose start failed is
skipped for 10 minutes and the board shows why. `taskmoor keep 0` turns it
off; running sessions go on. A manual Run is never held back by it, so it
can take the project one past N. An automatic start whose agent fails
after the claim (no answer from the agent, a refused sign-in) leaves the
task claimed with the session in error and its slot free: Restart it with
a profile, or Release it.

## The dispatcher (stage 4)

With `dispatcher_profile = "NAME"` (a claude profile; a cheap model is
enough) in the config, an agent's question is first looked at by the
dispatcher (C3):

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
refuse unknown keys); the same goes for `usage_threshold` before going
back past stage 6.

## The dispatcher's profile (stage 13)

With the dispatcher on, a task ready to run whose profile would come from
the default (`routing.default` or `default_profile`) has **Ask the
dispatcher** in its Run panel when at least two profiles could run it:
runnable (the program found), not billing an API key (so never OpenCode),
of the task's `harness` if it names one. Its short call reads the task, as
on the main branch, and those profiles (their harness, model,
`description` and the plan's usage left) and suggests one with a reason.
The board preselects it; you press Run (or pick another, which is then
your pick). A session started with the suggestion says "chosen by the
dispatcher" and why. A task's own `profile` or `model` and
`routing.<kind>` are never second-guessed; Keep N busy and `taskmoor run`
route as before. A suggestion lasts while the task file on main, the
project's checkout and those profiles are unchanged and the service runs;
Run refuses one that went out of date. Before going back past stage 13,
remove `description` from every `[[profiles]]` entry (older versions
refuse unknown keys), and after restarting reload open boards, on the Mac
and on paired devices (a stage 13 board sends a field older versions
refuse).

## Review and acceptance (stage 5)

When a session's turn ends, the task (`review`) gets
`min(reviews.code, 2)` code-review rounds by revust (`review_command`,
default `revust` on `PATH`; `review_profile`, default `codex-only`; its
archives in the service store, never in the worktree). Each round's
findings go back into the same session; it fixes, finishes, and the next
round runs; the fixes after the last round are marked unreviewed.
`reviews.code: 0` skips review. `taskmoor acceptance ID [--diff]` (or the
card's Acceptance) shows the commits, the diff against the merge base,
what is uncommitted in the worktree, the task's Checks, the agent's
summary and the review; `taskmoor accept ID --tip C` (Accept; C from the
acceptance view) merges:

- a trial merge first (`git merge-tree`, which writes no worktree): a
  conflict sends the task back to its session to merge main into its
  branch;
- then, in your checkout, which must be on the project's main branch
  (taskmoor never switches it): the merge commit is made from the
  trial's tree with `Task: <ID>` (`git commit-tree`, so no merge option,
  strategy or hook of your config changes it), your files move with
  `git read-tree -m -u` (it refuses when your changes are in the way),
  and main moves only from the commit that was checked; any path the
  merge writes that is untracked or ignored in your checkout refuses
  Accept beforehand (git itself would overwrite ignored files); nothing
  runs while your checkout is in a merge, rebase, cherry-pick or revert;
  `taskmoor accept ID --tip C` merges only the commit C you looked at;
- the task is `merged`, its state file goes; the worktree and the branch
  stay for you. **Push stays yours.** Send back (or `taskmoor say`)
  returns it to the session with an instruction.

## Codex and OpenCode sessions

Questions arrive as forms (Codex asks in its Default mode with
`default_mode_request_user_input`); approvals as ACP permission requests,
through the same rules as Claude's; what the rules cannot judge (more
sandbox access, a network destination, a command outside the worktree, a
fetch, a rename without its destination, another kind) always comes to
you. Codex runs in its own workspace-write sandbox (writes in the
worktree, no network, reads not confined; shown as "codex sandbox").
OpenCode has no sandbox ("unconfined"); its shell commands, edits outside
the worktree and fetches are asked, its own and the repository's
configuration are not loaded (MCP servers, plugins). Continue in terminal
gives `codex resume ID` or `opencode --session ID`. Your Codex
configuration (`~/.codex`, hooks, MCP servers, AGENTS.md) applies to Codex
sessions.

## The agent's rules

Every command, edit and fetch comes to the service (ask rules beat the
allow rules of your Claude settings), in Claude's default permission mode.
Push, publish, deploy (also `kubectl`, `helm`, `terraform` changes),
switching to the main branch and git writes in the main checkout are
refused without asking (also as Claude deny rules). Reads of secret files
wait in the inbox. File-tool writes outside the worktree, secret files,
dependency manifests, and in commands, output redirections and
copy/move/mkdir/… targets outside the worktree (temporary directories
excepted) and deletions outside it wait in the inbox; everything else is
allowed. The checks of a shell command are best effort: they read the
command, not what it runs.

## Claude's sandbox (stage 2b)

A Claude profile runs the agent's commands in Claude's sandbox (Seatbelt
on macOS; checked on macOS only) unless it sets `sandbox = false`. It
needs Claude Code 2.1.330 or newer; an older one is refused at Run.

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

## Billing

API-key variables are removed from the agent's environment, a Claude
settings file with `apiKeyHelper` refuses the run, and a session whose
`init` does not say `apiKeySource: none` is stopped, unless the profile
sets `api_billing = true`.

## Usage and power (stage 6)

Every 5 minutes the service reads how much of each subscription plan is
left: Claude's from the OAuth token Claude Code keeps in the login
Keychain and the undocumented `oauth/usage` endpoint (the token stays in
memory; macOS may ask once to allow taskmoor to read the item), Codex's
from `codex app-server` (Codex keeps its token). `taskmoor usage` and the
board's top bar show them. Below `usage_threshold` (default 10 percent
left; 0 turns the guard off) automatic starts wait — Keep N busy, review
rounds (on Codex's plan) and the dispatcher — and Run, Say (when it
resumes) and Restart need `--anyway` (the board offers "… anyway").
Running sessions are not touched; a profile that bills an API key (allowed
and a key present) has no plan to guard; a plan never read does not stop
anything, and one whose read fails later keeps its last figures until
their windows reset. Review rounds are guarded on the plan of revust's
`review_profile` (codex-only: Codex), and a postponed round runs when the
plan is back above the threshold. While any agent runs the service holds
macOS's idle-sleep assertion (`caffeinate -i`, ended with the service) —
sessions, review rounds and the dispatcher's calls count; the board says
"agents running — keep the Mac awake" (or why the assertion could not be
held): a lid closed on battery still sleeps.

## Telegram (stage 7)

Questions and approvals reach your Telegram: create a bot with @BotFather,
then `taskmoor telegram setup` (the token is read from stdin, kept in the
store with mode 0600, never shown) and `taskmoor telegram pair` (a
one-time code, 10 minutes: open the printed `t.me` link and press Start).
Only that account is heard; anyone else is ignored. The service polls
Telegram (outbound only). A simple question — one, single choice, not
secret — comes with option buttons, the recommended one first (★);
everything else, and every approval, comes with the link to the board on
your Mac, where it is answered (dangerous approvals are answered on the
Mac only; their message says only that one waits, nothing of the command).
Codex and OpenCode sessions can read the store's files, the bot token
included. A question the dispatcher may answer is sent once it is yours.
An ask answered anywhere closes its message; one still waiting after 30
minutes is sent again, and nothing is ever answered for you.
`taskmoor telegram status` and `off`. Question texts pass through
Telegram's servers.

## Ideas and intake (stage 8, v2)

`taskmoor idea add --title T TEXT` (or **+ New idea** in the board's Ideas
column) keeps a raw idea in the service's store, outside git.
`taskmoor idea grill ID` starts an agent on a branch `intake/<id>` in its
own worktree: it reads the repository, asks you what it needs to know in
forms (Needs you and Telegram; never the dispatcher), then writes the
agreed tasks under `tasks/` and commits them.
`taskmoor idea review ID [--diff]` shows the tasks it adds, as committed,
their validation and the diff; `taskmoor idea approve ID --tip C --main M`
(both as the review prints them) lands them on the local main as one
commit (only task files in `tasks/` and `tasks/tasks.toml`, and only if
they validate at that commit; the intake's own history stays on its
branch; if main has moved, the branch goes back to its session to be
brought up to date first). Push stays yours; the worktree and branch stay.
`taskmoor idea list | show | drop`.

## The web from outside (stage 9, v2)

The phone opens `https://<remote_host>/`: Caddy on your VPS asks for your
passkey (Pocket ID through oauth2-proxy), then a reverse ssh tunnel from
the Mac reaches a second listener of `taskmoor serve` on
`127.0.0.1:<remote_port>`. That listener knows no sign-in link and no API
token: a device is paired once with `taskmoor device pair` (a code shown
on the Mac, 10 minutes) and creates a passkey for `remote_host`; every
Accept, Approve and allowed request from it is confirmed with that passkey
again (a deny is not). Opening Finder or an editor, Telegram's setup and
devices are the Mac's only. `taskmoor device list | revoke ID`.
`taskmoor remote files --vps USER@HOST --auth-domain D` writes the Mac's
launchd agent for the tunnel and the VPS's Caddyfile, docker-compose.yml
(Caddy, Pocket ID, oauth2-proxy, pinned versions), .env (with new secrets,
mode 0600) and the restricted authorized_keys line, with a README of the
steps; it installs and deploys nothing.

## The board as a phone app (stage 10)

From outside the board installs as an app (Add to Home Screen): a
manifest, an icon and a service worker that keeps the board's own files
for when there is no network (the API is never cached; the last view
shows, marked Disconnected). On a phone the layout is one column with the
panel on the whole screen. In Needs you, **Notify me** subscribes the
paired device to Web Push: a question or approval for you, and each
reminder, arrives as a notification that opens the inbox. The message is
encrypted on the Mac for that browser (RFC 8291) and signed with the
service's VAPID key (RFC 8292, kept in the store); an approval's says only
that one waits. On an iPhone this needs the board on the Home Screen (iOS
16.4 or later). Revoking a device stops every new notification to it; one
its push service had already accepted may still arrive while the phone was
offline (they are kept 6 hours at most). **sign out** (from outside)
forgets the board on the device and ends the VPS's sign-in on a page of
its own.

## Cleaning up (stage 12)

Accepted tasks' and approved ideas' worktrees stay (the service never
removes one on its own). `taskmoor cleanup ID` or `taskmoor cleanup` (all;
`--project P`, `--dry-run`) — or **Clean up the worktree** on the card, on
the Mac — moves each worktree whose work is in main (the branch in main;
an idea's at the commit Approve landed), with no change git shows, whole
into the repository's trash beside it (`.<repo>-taskmoor-trash/`) and
deletes its branch. Nothing in a worktree is deleted: whatever git cannot
see there is in the trash. `taskmoor cleanup --empty-trash` deletes the
trash. What it keeps it names, with why.
