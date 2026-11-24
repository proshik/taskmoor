# taskmoor — agent conveyor

Status: **proposal; decisions answered by the person 2026-11-19…22; spec
review done 2026-11-22.** The review (revust `codex-only`, architecture lens,
one round) raised 11 findings; all were checked and fixed, marked
"review r1" in the decisions they changed.
Agent decisions A1–A30 (2026-11-22) close the open questions, and the
[Verify before code](#verify-before-code) checks were run on 2026-11-22;
the person may override any agent decision.
**Phase 0 is implemented (2026-11-22). C12 (2026-11-22): the conveyor is
built standalone, without Guild; v1 is split into the stages under
[v1 in stages](#v1-in-stages-without-guild), agreed by the person
2026-11-23; stages 1–7 of v1 are implemented, and stages 8 and 9 of v2 (ideas and intake; the web from outside), and stage 10 (the phone app, first of Later).**

This document extends [`plan.md`](plan.md). The plan's MVP (format, board,
`claim`/`review`/`release`) is the conveyor's foundation, **phase 0**; nothing
here changes D1–D5.

## Goal

taskmoor grows from a task board into a local service that:

- takes ready tasks and runs background agent sessions (claude, codex,
  opencode with custom models), each in its own worktree;
- routes the agents' questions either to a **dispatcher** that answers them or
  to the person as **forms with a recommended option**;
- runs review, and lets the person accept and merge;
- is reachable from a browser, Telegram and later a phone.

The person stays accountable for every task; the conveyor removes the manual
supervision loop the `orchestration` skill does by hand today.

## Sources

### Where the idea comes from

1. **Radio-T podcast 1038 (2026-11-14), bobuk (Grigory Bakunov), ~02:50–06:30.**
   His cycle: raw idea → "grilling" → tickets (formerly with skills). He wrote
   an app where the dialogue happens through forms (agree / disagree, why)
   instead of chat; ideas are cut into tickets on a kanban; "take these 5, run
   5 agents" moves tickets Ready → Running by itself, any agents underneath
   (he ran 2 Claude + 4 Pi). Differences from Linear he named: it chooses the
   right model for the situation and answers agents' trivial questions that
   the documentation already answers — "a conveyor manager", "more modern than
   a Software Factory".
2. **Linear** (linear.app, researched 2026-11-19). An issue is delegated to an
   agent (`delegate`; the person stays accountable). Agent Session states
   `pending, active, error, awaitingInput, complete, stale`; activities
   `thought, action, elicitation, response, error, prompt`; a question is an
   `elicitation` with a `select` signal (options, free text allowed); a `stop`
   signal; agent plans (steps `pending/inProgress/completed/canceled`, replaced
   whole); workspace/team guidance in Markdown passed to the agent; a Triage
   inbox (accept / decline / duplicate / snooze).
   <https://linear.app/developers/agents>,
   <https://linear.app/developers/agent-interaction>,
   <https://linear.app/developers/agent-signals>,
   <https://linear.app/docs/triage.md>.
   Not used as the board: tasks live in its cloud, not git; Claude is
   reachable only through MCP; its coding agents run in cloud sandboxes;
   webhooks need a public URL. The conveyor borrows its session and activity
   model.
3. **Guild**. Every agent session is an
   interactive TUI in a PTY tab; there is no headless mode. Status comes from
   OSC 7501 (`idle/working/blocked/done/error` with `kind`) and
   `guild agent list --json` (`idle/working/waiting/done/failed/unknown`,
   `kind`, `exited`); the question text of a waiting agent is not exposed.
   Supervision today is the `orchestration` skill: the parent writes an
   assignment file, starts a visible worker with
   `guild worktree create … --from-file --harness`, and loops
   wait/read/diff/send. Guild's `AGENTS.md` allows non-visible workers only
   with "real human question, tool-approval and resume channels" — exactly
   what the conveyor provides. Related backlog: GLD-017, GLD-018, GLD-049,
   GLD-010…016 (see [Relation to Guild](#relation-to-guild)). The task format
   taskmoor owns is Guild's `tasks/README.md` with `task.schema.json` and
   `state.schema.json`.
4. **agterm** (its plans
   `docs/plans/20261106-linux-remote-server.md`,
   `docs/plans/completed/20260831-remote-sessions.md`). Remote access entirely
   over the user's own ssh with key auth (`BatchMode=yes`, a `ControlMaster`
   pool per host), no own auth or relay; commands run the far side's CLI over
   ssh; a catalog subscription streams a snapshot plus epoch/revision-numbered
   deltas; Disconnected is never shown as Ended; asks opened with no client
   wait with a deadline and are offered to the next client that connects; a
   Go relay + SQLite draft was dropped as unnecessary.

### Facts the design rests on (checked 2026-11-19…22)

| # | Fact | Source |
| --- | --- | --- |
| F1 | `codex exec` forces `approval_policy = Never` and can ask nothing; `opencode run` cancels every question ("This non-interactive run cannot ask the user questions"). Background sessions that ask need long-lived modes. | harness docs/behaviour |
| F2 | `claude -p --input-format stream-json --output-format stream-json --verbose`: one long-lived process per session; events `system/init`, assistant/user messages, `result` with `total_cost_usd`, `usage`, `session_id`; flags `--resume`, `--session-id`, `--fork-session`. | <https://code.claude.com/docs/en/headless> |
| F3 | Claude questions: `AskUserQuestion` (`questions[]{question, header ≤12, options[2–4]{label, description, preview?}, multiSelect}`) via a permission host (`--permission-prompt-tool`) or a `PreToolUse` hook returning `permissionDecision:"defer"` (only in `-p`, single tool call in the turn) → the run ends with `stop_reason:"tool_deferred"` and is resumed later with `--resume` and the answer as `updatedInput`, so no live process is held while the person thinks. `AskUserQuestion` is not available in subagents. | <https://code.claude.com/docs/en/hooks.md> |
| F4 | Claude billing: `claude -p` uses the Pro/Max subscription login by default, but in `-p` an `ANTHROPIC_API_KEY` (also `ANTHROPIC_AUTH_TOKEN`, `apiKeyHelper`) wins, and `--bare` never reads the subscription. The Agent SDK docs: "Unless previously approved, Anthropic does not allow third party developers to offer claude.ai login or rate limits for their products, including agents built on the Claude Agent SDK" — hence Claude does not go through the SDK-based ACP adapter. | <https://code.claude.com/docs/en/authentication> |
| F5 | `codex app-server` (experimental): JSON-RPC over stdio/unix/ws; `thread/start|resume|fork`, `turn/start|steer|interrupt`; server requests `item/commandExecution/requestApproval`, `item/fileChange/requestApproval`, `item/tool/requestUserInput` (EXPERIMENTAL; `questions[]{id, header, question, options[]{label, description}, isOther, isSecret}`). The model gets the request-user-input tool only in Plan collaboration mode unless the under-development feature flag `default_mode_request_user_input` is on — **the main risk of the whole design** (checked: F15, A26). Custom providers need `wire_api = "responses"`. | Codex docs |
| F6 | codex-acp (ACP adapter over app-server) supports the ChatGPT subscription locally ("requires paid subscription and doesn't work in remote projects"); `zed-industries/codex-acp` is archived and moved to `agentclientprotocol/codex-acp` (read 2026-11-22, F17). | codex-acp README |
| F7 | OpenCode v2: `opencode acp` speaks ACP natively over stdio, one process many sessions, load / list / resume / fork; questions are sent only if the client advertises `elicitation.form`. `opencode serve` HTTP API: forms (`form.created`, reply `{answer}`, fields with `options`, `default`), permissions (`once|always|reject`), cost. Providers: ChatGPT Plus/Pro and Copilot by subscription, custom openai-compatible models by the person's keys; Anthropic subscription login is not offered. | <https://opencode.ai/docs/acp/>, <https://opencode.ai/docs/server/>, <https://opencode.ai/docs/providers/> |
| F8 | ACP: JSON-RPC 2.0, v1 stable (v2 draft since 2026-07-20); `session/new|load|resume|list|close|prompt|cancel`, `session/update` (chunks, tool calls, plan, `usage_update`), `session/request_permission` (allow/reject once/always), `elicitation/create` (form mode, flat JSON Schema, accept/decline/cancel). **Transport is stdio only** (HTTP/WS is an RFD), so remote access is the service's own API, never ACP. | <https://agentclientprotocol.com> |
| F9 | No harness has a "recommended option" field: Claude and Codex put it first, conventionally labelled "(Recommended)"; OpenCode uses the field's `default`. The service normalizes. | F3, F5, F7 |
| F10 | vibe-kanban (<https://github.com/BloopAI/vibe-kanban>) is the closest existing product (board, worktree per task, many agents); third-party pages report it is shutting down (**not verified at the primary source**). It confirms the idea; it is not a dependency. | GitHub, third-party pages |
| F11 | Guild reads plan usage in `crates/desktop/src/usage.rs`: Claude's from the OAuth token Claude Code keeps in the login Keychain and the **undocumented** `https://api.anthropic.com/api/oauth/usage`; Codex's by running `codex app-server`. | Guild source, read 2026-11-22 |

Checked on this Mac on 2026-11-22 (Claude Code 2.1.330, codex-cli 0.171.0,
opencode v2.2.5); the probes are described under
[Verify before code](#verify-before-code):

| # | Fact | How |
| --- | --- | --- |
| F12 | `claude -p` with no API-key variables runs on the subscription: `system/init` carries `apiKeySource: "none"`. `result.total_cost_usd` is reported even then (a notional figure). | live run |
| F13 | In `claude -p` **`AskUserQuestion` is not in the tool list** — not with `--input-format stream-json`, not with `--tools default,AskUserQuestion`. It appears only with a permission host: `--permission-prompt-tool stdio` (the host answers on the stream). | live runs |
| F14 | With that host, a `PreToolUse` hook on `AskUserQuestion` returning `defer` ends the run with `stop_reason` and `terminal_reason` `tool_deferred`. `claude -p --resume <id>` (no new prompt) with a hook returning `permissionDecision: "allow"` and `updatedInput` = the original input plus `answers: {<question>: <label>}` delivers the answer; the session id stays the same. The model put the recommended option first, labelled "(Recommended)". | live runs |
| F15 | Codex in Default mode has **no** request-user-input tool (the model answered "NO_TOOL"). In Plan mode (`thread/settings/update` with `collaborationMode.mode = "plan"`) it asks: `item/tool/requestUserInput`, `isBlocking: true`, `autoResolutionMs: null`, the recommended option first with "(Recommended)". With `--enable default_mode_request_user_input` it asks in Default mode too, `isBlocking: false`, and the turn still waited for an answer sent 25 s later and used it. | live `codex app-server` probes |
| F16 | Codex's thread id is its session id (`session_meta.id` in `~/.codex/sessions`), and `codex exec resume <thread id>` continued the app-server conversation with its context. codex-acp uses the thread id as the ACP session id (`threadId: request.sessionId`). | live run; codex-acp source |
| F17 | `agentclientprotocol/codex-acp` is alive (TypeScript, last push 2026-11-21, read at `e3a679f19345`). It maps `item/tool/requestUserInput` to ACP `elicitation/create` **only if the client advertises form elicitation, and otherwise answers with no answers**; exposes Plan/Default as the session config option `collaboration_mode`; also handles Codex's `request_user_input_async` (questions that do not stop the turn; an answer after the turn ends is dropped); auth methods: ChatGPT, API key (`CODEX_API_KEY` over `OPENAI_API_KEY`), custom gateway. | source |
| F18 | `opencode acp` (v2.2.5 source): the ACP session id *is* the opencode session id (so `opencode --session <id>` resumes it). A form is sent as `elicitation/create` only when the client advertises `elicitation.form`, the form is a question (or web-search provider choice), and it has no credential-like, external or conditional fields; otherwise opencode cancels it and tells the model to "continue without an answer". A field's `default` becomes the JSON Schema `default`. On this Mac opencode's only provider is **OpenCode Console by API key**. | source; `opencode auth list` |
| F19 | `https://api.telegram.org/` answers from this Mac (HTTP 302 in 1.5 s). | `curl` |
| F20 | git 2.50.1 has `merge --no-overwrite-ignore` and `--no-autostash`; `git merge-tree --write-tree main <branch>` exits 1 on a conflict and leaves the worktree, the index and `MERGE_HEAD` untouched. | throwaway repository |

## Decisions

Answered by the person one by one; not re-opened here.

- **C1 — where (2026-11-19).** taskmoor grows: a Go service
  `taskmoor serve` + web UI + the planned TUI; Guild is one of the clients.
  Not inside Guild, not Linear as the board.
- **C2 — agents (2026-11-19, amended 2026-11-22).** codex through codex-acp
  and opencode through `opencode acp` (ACP); **Claude through native
  `claude -p` stream-json**, not the SDK-based adapter, because of the
  subscription policy (F4). The service strips `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN` and `apiKeyHelper`-style overrides from an agent's
  environment unless the profile explicitly allows API billing.
- **C3 — dispatcher, three levels (2026-11-19).**
  (a) the answer exists in AGENTS.md / decisions / specs / the task → answer
  automatically with a citation;
  (b) technical, reversible, inside the task's scope → pick the recommended
  option, mark it "answered by the dispatcher", the person can override;
  (c) scope, visible UI, new dependencies, push/merge/publish, deletions,
  secrets → a form to the person with the recommended option first.
  Never answer an approval that is the person's (consistent with GLD-049 and
  the person's rules).
- **C4 — one service for all projects (2026-11-22).** One board with a
  project switcher and one shared inbox. Task files stay in each project's
  `tasks/`; task state stays in that project's `<git-common-dir>/tasks/state/`;
  the service keeps ideas, session journals and dispatcher decisions in its
  own state directory. The web is only a UI: agents are CLI processes on the
  Mac under the person's own logins, so billing is the person's
  subscriptions. The Mac must stay awake while agents run (the service holds
  a power assertion only then).
- **C5 — remote access (2026-11-22).** No Tailscale (the person keeps an
  anti-block VPN on phone and laptop). The Mac holds a reverse `ssh -R`
  tunnel to the person's VPS by key (agterm's idea: reuse ssh, no own auth for
  this leg; launchd keeps it up); Caddy on the VPS gives HTTPS and checks a
  passkey login *before* the tunnel; the service also checks a per-device
  token (paired once with a code shown on the Mac, revocable); at home plain
  `localhost`; a laptop may use `ssh -J`. Protocol for every client borrowed
  from agterm: snapshot + revisioned deltas, Disconnected ≠ Ended, a pending
  ask waits with a deadline for the next client.
- **C6 — notifications (2026-11-22).** A Telegram bot: outbound polling from
  the Mac, no inbound path; sends a question with option buttons (recommended
  first) and a link to the full form; simple questions can be answered in
  Telegram; only the person's Telegram account is accepted. Question text
  passes through Telegram's servers (accepted by the person).
- **C7 — dangerous approvals remotely (2026-11-22).** Allowed, but each one
  needs a repeat passkey/Face ID; from Telegram the button opens the web page
  for it.
- **C8 — ideas (2026-11-22).** Raw ideas and grilling transcripts live in the
  service's store, outside git; only approved tasks and their agreement reach
  git.
- **C9 — intake into main (2026-11-22).** A proposal is prepared on an
  `intake/<id>` branch in its own worktree; "Approve" merges it into the local
  main with git, which refuses when the person's uncommitted edits touch those
  files; push stays the person's.
- **C10 — model routing (2026-11-22).** Profiles plus a table by task kind in
  `tasks.toml`; a field in the task overrides it; the choice is shown and
  changeable at launch. Automatic choice by the dispatcher is not in the
  design (Later).
- **C12 — standalone, without Guild (2026-11-22).** The person splits
  Guild into a server and clients meanwhile; the conveyor must work on its
  own first, so the whole can be seen separately. Nothing in v1 needs Guild
  running, its CLI or its socket. Guild later becomes one more client of
  the service API (like the web and the TUI). Where this document names a
  Guild action in v1, the standalone replacement in
  [v1 in stages](#v1-in-stages-without-guild) applies.
- **C13 — confining the agents (2026-11-22).** Stage 1 runs agents as the
  person, so their subprocesses can use the person's credentials and the
  service's API token. Decided: confine them with Claude Code's own sandbox
  (file and network limits for Bash) in the agent profiles, as a separate
  stage right after stage 2, checked on a real session; a separate macOS
  user was not chosen.
- **C11 — scope of the first version (2026-11-22).**
  **v1:** the run loop + Telegram — board (web on localhost + TUI), launching
  ready tasks into worktrees, background agents, the inbox with forms, the
  dispatcher, revust review, acceptance and merge, the Telegram bot.
  **v2:** ideas and grilling through forms (with "Continue in terminal"), the
  web outside through the VPS.
  **Later:** mobile app (PWA first), dispatcher model choice, Linux-server
  mode like agterm-server (deferred because Guild builds only on macOS),
  Guild UI buttons (need Penpot first), attaching to a background session's
  live terminal.

## What the person sees

### Install and configure

- One binary. `taskmoor serve` runs in the background and serves the web on
  `localhost`; `taskmoor` alone is the TUI board.
- Agent **profiles** are configured once in the service's config: a name, a
  harness (`claude`, `codex`, `opencode`), a model, for opencode custom
  models the provider, and whether API billing is allowed (default: no).
- Projects are registered once (a repository path); the board's project
  switcher lists them. The exact command/UI is to be settled in
  implementation.

### Board

| Column | What it holds | Source |
| --- | --- | --- |
| Ideas (v2) | raw ideas and grilling in progress | service store (C8) |
| Backlog | lifecycle `backlog`, not ready | plan: file + git + state |
| Ready | lifecycle `backlog` and ready | plan's readiness: `depends_on`, `after_release`, `touches` overlap with `in_progress`/`review` (D4) |
| Running | lifecycle `in_progress` | state file |
| **Needs you** | derived: running with a pending question routed to the person | service: session journal |
| Review | lifecycle `review` (agent finished; revust rounds; acceptance) | state file |
| Merged | `Task: <ID>` on main's first-parent history | git |
| Done | release rule of the component | git |

Every agent card carries a billing badge, **subscription** or **API key**,
and the board shows the remaining plan usage per harness.

### Launch

The person picks tasks and presses **Run**, or sets **keep N agents busy**.
For each task the service:

1. shows the chosen profile/model (from the task, else the routing table)
   and lets the person change it before start (C10);
2. creates the worktree on branch `task/<ID>-<slug>` from the local main;
3. `claim`s it (plan's semantics: atomic create-if-absent of the state file,
   readiness re-checked under the per-project lock, A29);
4. starts the agent with the task file's body as its assignment — what the
   `orchestration` skill does by hand today;
5. as the launcher, writes the `harness` record (`name`, `status`, `kind`,
   `session`, `observed_at`) into the state file as the session changes — the
   format gives that to the launcher (A1).

"Keep N busy" takes Ready tasks by priority then id, re-derives readiness
after every claim (a claimed task now holds its `touches`), and starts
nothing while a usage or billing guard says no.

### While running

- The card's **session feed** in Linear's activity model: thoughts, actions,
  the plan, changed files, answers, errors.
- **Stop** (the `stop` signal); **Add an instruction** (a `prompt` activity
  sent to the same session); **Open the worktree in Guild**.
- **Continue in terminal**: resumes the same conversation in Guild —
  `claude --resume <id>`, `codex resume <id>`, `opencode --session <id>`.
  The ids match: Claude's session id is its own; Codex's ACP session id is
  its thread id and `codex resume` continues it (F16, verified live);
  opencode's ACP session id is its session id (F18, from source).

### Questions

Every question goes to the dispatcher first. Levels (a) and (b) are answered
and logged with the reason and shown in the feed as "answered by the
dispatcher"; level (c) and the person's approvals go to the **inbox** and to
**Telegram** as a form with the recommended option first.

### Review and acceptance

1. The agent finishes → the service runs `taskmoor review` semantics
   (lifecycle `review`) → the card moves to Review.
2. The service runs **revust with the `codex-only` profile** on the branch.
   Findings go back into the same session as a prompt; the agent fixes; at
   most **2 code-review rounds** counting the first. After that the task
   comes to the person whatever the result.
3. **Acceptance view:** the diff against the base, the task's Checks and
   the agent's own summary of what it ran (taskmoor does not run the
   checks itself), the review result. **Accept** merges the branch into
   the local main with the `Task: <ID>` trailer in the merge commit (the
   format's merge rule), after a trial merge that leaves the person's
   checkout untouched on conflict (A8). Push stays the person's.

### Guards

- **Usage:** limits are shared with the person's own work. The service does
  not start new agents when a plan's limit runs low and shows the remainder on
  the board (read as Guild reads it, F11).
- **Billing:** an agent that would bill an API key is not started unless its
  profile allows it.

## Architecture

```
                     clients
  web (localhost / VPS)   TUI   Telegram bot   Guild
            │              │         │           │
            └──────── service API: snapshot + revisioned deltas, commands ──┐
                                                                            │
 taskmoor serve ────────────────────────────────────────────────────────────┘
   project registry ── per project: plan's loader/derive/readiness (phase 0)
   scheduler ── usage guard ── billing guard ── power assertion
   runners: claude (stream-json) │ codex (codex-acp) │ opencode (opencode acp)
        │ events                     ▲ answers / prompts / stop
        ▼                            │
   session journal (activities, session state) ── dispatcher ── inbox
   review stage (revust codex-only, ≤2 rounds) ── acceptance / merge
   state-file writer (<common>/tasks/state/<ID>.json, the launcher's fields)
   service store (journals, decisions, inbox, ideas, devices, config)
```

| Component | Responsibility |
| --- | --- |
| Project registry | Registered repositories; per project the phase-0 snapshot (tasks from the main branch's tree per D1, state files, git facts), refreshed by the plan's polling fingerprint. |
| Scheduler | Run / keep N busy; readiness from phase 0; worktree + branch + `claim`; picks the profile (C10); asks the guards; holds a power assertion while any agent runs (mechanism to be settled in implementation). |
| Runners | One per harness (see [Agent integration](#agent-integration-per-harness)); start, prompt, answer, stop, resume; translate harness events into journal activities; build the agent's environment (billing guard). |
| Session journal | Append-only record per session: session state + activities. The feed, the "Needs you" column and the state file's harness record all come from it. |
| Dispatcher | Classifies each question into (a)/(b)/(c), answers (a)/(b), routes (c) and the person's approvals to the inbox; writes an audit record. |
| Inbox | Pending asks across all projects; delivery to connected clients and Telegram; deadlines (C5). |
| Review stage | Runs revust `codex-only` on the branch when the session completes; feeds findings back; counts rounds (≤2). |
| Acceptance / merge | Acceptance view; trial merge with `git merge-tree`, then `git merge --no-ff --no-overwrite-ignore --no-autostash` into the local main with `Task: <ID>`; git refuses on the person's touching uncommitted edits (A8). |
| State-file writer | The plan's atomic writes (`claim` link, `review` rename) plus the launcher's harness record updates. One writer per task. |
| Usage guard | Reads remaining plan usage per harness (F11); blocks new starts below a threshold (value to be settled); shows the remainder. |
| Billing guard | Decides the badge per agent; strips API-key overrides (C2); refuses a Claude profile whose settings set `apiKeyHelper` and stops a session whose `init` is not `apiKeySource: none` (A30); refuses to start API billing without the profile's permission. |
| Clients | **Web**: board, feed, inbox forms, acceptance. **TUI**: the plan's board as a client of the service. **Telegram**: notifications and simple answers. **Guild**: opens worktrees, "Continue in terminal". |

### Session journal and its activity model

Session states (Linear's): `pending`, `active`, `awaitingInput`, `error`,
`complete`, `stale`. Activities: `thought`, `action`, `elicitation`,
`response`, `error`, `prompt`; plus the agent's **plan** (steps
`pending/inProgress/completed/canceled`, replaced whole) and **usage/cost**
records. An `elicitation` carries the normalized question (below) and, once
answered, the answer and who gave it (dispatcher level, or the person).

Mapping to the state file's `harness.status` (proposed; to be settled in
implementation):

| Journal | `harness.status` |
| --- | --- |
| `pending` | `unknown` |
| `active` | `working` |
| `awaitingInput` (question / approval) | `blocked` + `kind: question` / `permission` |
| `complete` | `done` |
| `error` | `error` |
| `stale` | `unknown` |
| runner process ended unexpectedly | `exited` (+ `exit_code`) |

### Normalized question

The service turns every harness question into one shape: `questions[]` with
`header`, `question`, `options[]{label, description, preview?}`,
`multiSelect`, `allowOther`, `secret`, and `recommended` (an option index or
none). "Recommended" is derived per harness (F9): Claude and Codex — the
option labelled "(Recommended)", the label stripped for display; OpenCode —
the field's `default`. Clients show the recommended option first. An approval
(`session/request_permission`, Codex approvals, Claude tool permissions) is a
separate kind with the harness's own choices (allow/reject, once/always).

## Agent integration per harness

| | Claude | Codex | OpenCode |
| --- | --- | --- | --- |
| Mechanism | `claude -p --input-format stream-json --output-format stream-json --verbose` in the worktree; the service is the stream's peer (F2) | `codex-acp` over `codex app-server`; the service is the ACP client over stdio (F5, F6, F8) | `opencode acp`, native ACP over stdio; the client advertises `elicitation.form` (F7, F8) |
| Session id | given up front with `--session-id`, so `claim --session` can carry it | from `session/new` | from `session/new` |
| Question arrives | `AskUserQuestion` exists only with the stdio permission host (F13); a `PreToolUse` hook `defer`s it → run ends `tool_deferred` (F14) | `item/tool/requestUserInput` → codex-acp → `elicitation/create`, only because the service advertises form elicitation (F17); Default mode needs `default_mode_request_user_input` (F15, A26) | `elicitation/create` (form mode), only for credential-free question forms (F18) |
| Question answered | `claude -p --resume <id>` with a hook returning `allow` + `updatedInput.answers` (F14); the host's reply for in-turn cases | ACP reply to the elicitation; the live process waits (F15) | `accept` with the form values |
| Approvals | Claude tool permissions through the same hook/host | `session/request_permission` (from app-server's command/file approvals) | `session/request_permission` (`once/always/reject`) |
| "Recommended" | option labelled "(Recommended)" | option labelled "(Recommended)" | the field's `default` |
| Stop / instruction | to be settled in implementation (end the process; a new user message on the stream or `--resume` with a prompt) | `session/cancel`; `session/prompt` | `session/cancel`; `session/prompt` |
| Resume | `--resume <session_id>`; "Continue in terminal": `claude --resume` (proven) | `session/load`/`resume`; terminal: `codex resume <id>` — ACP id = thread id (F16, verified) | `session/load`/`resume`; terminal: `opencode --session <id>` — ACP id = session id (F18, from source) |
| Billing | subscription login by default; the runner strips `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `apiKeyHelper`-style overrides unless the profile allows API billing; never `--bare` (F4); refuses to start when Claude's settings set `apiKeyHelper` (A30); the badge comes from `system/init.apiKeySource` (`none` = subscription, F12) | the runner selects codex-acp's ChatGPT auth method (F17); `codex login status` says "Logged in using ChatGPT"; custom providers (`wire_api = "responses"`) are API billing | ChatGPT Plus/Pro, Copilot: subscription; custom openai-compatible models and OpenCode Console: API billing, so such profiles must allow it — on this Mac every opencode profile is API-billed today (F18) |
| Cost/usage | `result.total_cost_usd`, `usage` | `usage_update` | `usage_update` |
| Risks | `AskUserQuestion` absent in subagents and without the host; `defer` only with a single tool call in the turn | `default_mode_request_user_input` is "under development" (F15) and may change; app-server is experimental; a waiting question holds a live process | a form opencode will not show is cancelled silently (F18); ACP v2 is a draft |

Common rules:

- Background sessions use only long-lived modes; `codex exec` and
  `opencode run` are not used (F1).
- ACP is stdio only (F8): the service spawns ACP agents locally and never
  exposes ACP to clients; clients speak the service's API.
- Process-per-session versus one shared `opencode acp` process for many
  sessions: to be settled in implementation.
- Claude: the runner always passes `--permission-prompt-tool stdio` and is
  the permission host; `defer` is the path for questions (A15); how the hook
  reaches the service is to be settled in implementation.

## Dispatcher policy

### Levels

| Level | When | Action | Shown as |
| --- | --- | --- | --- |
| (a) | The answer exists in the project's AGENTS.md, recorded decisions, specs, or the task file | Answer automatically | "answered by the dispatcher" + citation (file, section) |
| (b) | Technical, reversible, inside the task's scope | Pick the recommended option | "answered by the dispatcher" + reason; the person can override |
| (c) | Scope, visible UI, new dependencies, push/merge/publish, deletions, secrets | Form to the person, recommended option first | inbox + Telegram |

### Hard stops

Push, publish and deploy commands from an agent are rejected without asking,
and the service never runs them itself (A11): push stays the person's. The
(c) categories are hard stops: a question or approval that touches any of
them goes to the person even when a document seems to answer it or an option
is marked recommended. An approval that is the person's is never answered by
the dispatcher (C3, GLD-049). How a question is classified (rules first, then
judgement) is A13; the hard-stop check always runs before (a) and (b).

### Audit record

Every dispatcher decision is kept in the service store:

| Field | Meaning |
| --- | --- |
| `id`, `at` | record id, time |
| `project`, `task`, `session` | where the question came from |
| `question` | the normalized question as asked |
| `level` | `a`, `b` or `c` |
| `answer` | the chosen option(s) or text; empty for `c` until the person answers |
| `citation` | (a): the document and section |
| `reason` | why this level and this answer |
| `answered_by` | `dispatcher` or `person` |
| `overridden` | the person's later answer, if any, and when |

### Override

The person sees every (b) answer in the feed and the inbox's history and can
override it. An override is delivered to the same session as an instruction
(a `prompt` activity naming the question, the dispatcher's answer and the
person's answer) and recorded in the audit record. The agent may already have
acted on the first answer; the instruction tells it to change course (C3
limits (b) to reversible questions for this reason).

## Storage

| Where | What | Writer |
| --- | --- | --- |
| git, each project | `tasks/*.md`, `tasks/tasks.toml` (with the routing table), the code; approved tasks and their agreement (C8, v2 via `intake/<id>`); merge commits with `Task: <ID>` | agents on task branches; the service's merge on Accept/Approve |
| `<git-common-dir>/tasks/state/<ID>.json`, each project | lifecycle `in_progress`/`review`, worktree, branch, base, times, writer, `harness {name, status, kind?, exit_code?, session?, observed_at}` — the existing `state.schema.json`, unchanged | the service as launcher (`claim`, `review`, harness record); one writer per task |
| service store (the service's own state directory, location to be settled) | project registry, profiles and service config, session journals, dispatcher audit records, the inbox and pending asks, ideas and grilling transcripts (v2), paired device tokens, Telegram settings, usage readings | the service only |

Nothing about a session's conversation goes into git or the state file
beyond the harness record. Secrets (Telegram bot token, device token hashes)
stay in the service store or the Keychain (to be settled).

## Remote access and the client protocol

### Paths

| Where the person is | Path |
| --- | --- |
| At the Mac | `http://127.0.0.1:<port>` (the address of the sign-in link) |
| Laptop elsewhere | `ssh -J <vps>` to the Mac, then a local forward to the service (details to be settled) |
| Phone / any browser (v2) | `https://<vps-domain>` → Caddy (HTTPS, passkey login) → the reverse tunnel → the service (per-device token) |

- The Mac keeps `ssh -R` to the VPS by key, `BatchMode=yes`, kept up by
  launchd (C5). The tunnel binds to the VPS's loopback; only Caddy reaches it.
- Caddy checks a passkey login before proxying; the service then checks a
  per-device token. A device is paired once with a code shown on the Mac and
  can be revoked from the Mac.
- Dangerous approvals from outside need a repeat passkey/Face ID for each one
  (C7, v2, A10). In v1 they need a Touch ID / password confirmation on the
  Mac itself, which a connection forwarded over ssh cannot give (A9).
- No Tailscale, no relay of our own, no inbound port on the Mac.

### Client protocol (borrowed from agterm)

- A client subscribes and gets a **snapshot** (projects, tasks with derived
  status, sessions, inbox) followed by **deltas** numbered by `(epoch, rev)`.
  A new epoch means the service restarted: the client drops its revision and
  takes the next snapshot whole.
- A client that loses the connection shows its last view as
  **Disconnected**, never as Ended; a session is Ended only when the service
  says so.
- A **pending ask** (question or approval) opened while no client is
  connected waits with a deadline and is offered to the next client that
  connects; Telegram is notified at once.
- Commands (Run, answer, stop, instruction, accept, approve) go over the same
  API. Wire format and transport (HTTP + a streaming channel) are to be
  settled in implementation.

### Telegram (v1)

- The bot polls Telegram from the Mac (outbound only).
- A question becomes a message with the question text, option buttons
  (recommended first) and a link to the full form. Simple questions are
  answered with a button; the rest through the form.
- Only the person's Telegram account is accepted; anything else is ignored.
- A dangerous approval's button opens the web page that asks for the passkey
  (C7, v2). In v1 the message says it waits for a confirmation on the Mac
  (A9).

## Changes to the task format

All additive. The proposal is to stay at **`format = 1`**, for the reason D2
used: no released reader or writer exists yet besides taskmoor itself, and
existing files stay valid. Because `task.schema.json` has
`additionalProperties: false` and the plan's `tasks.toml` validation rejects
unknown keys, a reader built before these fields would reject files that use
them. So `format = 1` holds only if the fields land in `spec/` before a
phase-0 build is used anywhere (A25 lands them with phase 0), and Guild's pinned copies
of the schemas are refreshed (Guild's edit). If phase 0 is released first,
they need a format bump instead.

### Task front matter (`task.schema.json`)

| Field | Required | Meaning |
| --- | --- | --- |
| `profile` | no | Name of an agent profile (`^[a-z][a-z0-9-]*$`) to run the task with; overrides the routing table. A profile the local service does not know is reported and the launch asks the person. |
| `model` | no | A model name that overrides the chosen profile's model for this task. |

The existing `harness` field (`any`, `claude`, `codex`, `opencode`; a
preference) stays as it is; how it combines with `profile` is A16.

### `tasks.toml` routing table

```toml
# Which agent profile runs a task of each kind (taskmoor conveyor, C10).
# Profile names are defined in the service's config, not here.
[routing]
default  = "codex-default"
feature  = "claude-main"
fix      = "codex-default"
research = "opencode-custom"
```

Keys are `default` and the format's kinds (`feature`, `fix`, `chore`,
`research`, `design`); values are profile names. The table is optional.
Choice order at launch: the person's choice in the launch dialog → task
`model` / `profile` → `routing.<kind>` → `routing.default` → the service's
default profile. The names above are examples.

`state.schema.json` needs no change: `harness.session` (D2) carries the
session id.

## Versions

| Version | Contents |
| --- | --- |
| Phase 0 | `plan.md`'s MVP: format in `spec/`, reading, derivation, readiness, validation, `claim`/`review`/`release`, TUI board; plus `profile`, `model`, `[routing]` in `spec/` (A25). |
| v1 | `taskmoor serve`; web on localhost; TUI as a client; project registry and switcher; launching ready tasks (Run, keep N busy) into worktrees; background agents through the three runners; session journal and feed; inbox with forms; dispatcher; revust review (`codex-only`, ≤2 rounds); acceptance and merge; usage and billing guards; Telegram bot. |
| v2 | Ideas and grilling through forms (with "Continue in terminal"); intake on `intake/<id>` and Approve into main (C9); the web outside through the VPS (C5, C7). |
| Later | Mobile app (PWA first); dispatcher model choice; Linux-server mode like agterm-server (deferred: Guild builds only on macOS); Guild UI buttons (need Penpot first); attaching to a background session's live terminal. |

## v1 in stages, without Guild

Agreed by the person 2026-11-22 (proposed under C12). v1 is too
big for one merge, so it is built in seven stages; each one ends with
something the person can run, test on fixtures and check by hand, and each
is reviewed like phase 0 (revust `codex-only`, at most 2 rounds). The
order puts one harness end to end first and widens later.

### Without Guild

| Where the design names Guild | Standalone in v1 |
| --- | --- |
| "Open the worktree in Guild" | **Open the worktree**: the card shows the path and copies `cd <worktree>`; the web offers `open <worktree>` (Finder) and the configured editor command. |
| "Continue in terminal" in Guild (A2) | The card shows the exact resume command for the session (`claude --resume <id>`, `codex resume <id>`, `opencode --session <id>`) in the worktree. Pressing **Continue in terminal** hands over: the service stops its runner, marks the session "continued in terminal" and removes `harness.session` from the state file, so the status is `unknown` until the task is handed back or reviewed (`taskmoor review` from the person's terminal works as in phase 0). |
| Harness status from `guild agent list` | Not used by the service: it is the launcher and knows its own sessions. The phase-0 Guild source stays in the CLI/TUI and is inert without `GUILD_SOCKET`; sessions the service runs always carry `harness.session`, so a Guild agent never shadows them. |
| Plan usage "read as Guild reads it" (F11) | Implemented in taskmoor itself (stage 6), with the same sources; Guild's code is only the reference. |
| Guild as a client | Later, over the same service API as the web and the TUI. |

### Stage 1 — the service and one Claude session

The smallest loop that runs a real agent on a real task.

- `taskmoor serve` runs in the foreground (launchd later), one instance per
  user (`flock` on the store), listening on `127.0.0.1` (port from the
  config, default `7733`). A random API token in the store (mode 0600)
  guards the API; CLI clients read it, browsers get it in stage 2.
- **Config and store.** `$XDG_CONFIG_HOME/taskmoor/config.toml` (default
  `~/.config/…`): projects, profiles (`name`, `harness`, `model`,
  `api_billing`), `default_profile`, port. Store
  `$XDG_STATE_HOME/taskmoor/` (default `~/.local/state/…`): session journals
  (JSONL, A18), pending asks, the token. `TASKMOOR_HOME` overrides both for
  tests.
- **Projects.** `taskmoor project add PATH`, `project list`, `project
  remove NAME` (the config only; nothing in the repository).
- **Run.** `taskmoor run ID [--project P] [--profile NAME]` asks the
  service to: re-derive readiness, create the worktree
  `<repo-dir>-<id-lowercase>` on `task/<ID>-<slug>` from the local main
  (A23), `claim` it under the lock with `--session` (writer
  `taskmoor serve <version>`, A6), start the runner with the task body as
  the assignment. A `needs_person` task needs `--person` (A4).
- **Claude runner** only (C2, A30): `claude -p --input-format
  stream-json --output-format stream-json --verbose --session-id <uuid>
  --permission-prompt-tool stdio`; the service is the permission host, and
  `AskUserQuestion` arrives as a `can_use_tool` request answered with
  `updatedInput.answers` (implementation decision 1 below, in place of
  A15's hook); API-key variables stripped; refuses a profile whose Claude
  settings set `apiKeyHelper` or an API key; stops the session if
  `system/init.apiKeySource` is not `none`.
- **Permissions without the dispatcher.** The fixed rules of A13 only:
  push/publish/deploy and merges into main are rejected without asking
  (A11); deletions outside the worktree, secret paths and dependency
  manifests go to the person; other tool calls inside the worktree are
  allowed. Everything the rules send to the person and every question waits
  in the **inbox**.
- **Journal and state.** Session states and activities as in
  [Session journal](#session-journal-and-its-activity-model); the state
  file's `harness` record follows them (the mapping table there). When the
  session completes, lifecycle becomes `review`; no revust yet.
- **CLI as the first client.** `taskmoor sessions`, `taskmoor feed ID
  [--follow]`, `taskmoor inbox`, `taskmoor answer ASK OPTION|--text T`,
  `taskmoor stop ID`, `taskmoor say ID TEXT` (an instruction), all with
  `--json`.
- **API.** HTTP + JSON: `GET /api/snapshot`, `GET /api/events` (server-sent
  events, deltas numbered `(epoch, rev)`, C5's protocol), `POST` commands.
- **Tests** with a fake `claude` (a test binary speaking recorded
  stream-json) on fixture repositories; no provider is called by tests. The
  real check runs one short subscription session on a throwaway repository,
  with the person's permission at that time.

#### Stage 1 — implementation decisions (2026-11-22)

1. **Questions are answered through the permission host, not by
   `defer` + `--resume` (changes A15 for stage 1).** `AskUserQuestion`
   comes to the host as a `can_use_tool` control request (as every tool
   that needs permission does) and is answered with `allow` and
   `updatedInput.answers` while the process waits. Fewer moving parts (no
   hook command, no resume per answer); the cost is a live process while
   the person thinks. The defer path stays possible for a later stage.
2. **One conversation per run.** When a turn ends (`result`) the service
   closes Claude's input, the session is `complete` and the task moves to
   `review`. `say` to an ended session resumes it with `claude -p --resume
   <id>` and sets the task back to `in_progress`; `say` to a running one
   adds a user message.
3. **Permission mode and hard stops.** The agent inherits the person's
   Claude settings and instructions. The runner passes `--permission-mode
   default` (so a person's auto or bypass default does not skip the host),
   the A11 commands as `--disallowedTools` deny rules (they win over allow
   rules), and stops a session whose `init` says `bypassPermissions` (a
   managed setting). The host's rules (`internal/serve/policy.go`) catch
   what the prefixes miss.
4. **The fixed rules only (A13's first step).** Refused: push, publish,
   deploy, releases, PR merges, `git checkout|switch <main>`. Asked: writes
   outside the worktree, secret paths, dependency manifests, deletions
   outside the worktree, commands naming the main checkout. Allowed:
   everything else. The dispatcher's judgement is stage 4.
5. **States and the harness record.** Added `stopped` (the person's Stop):
   the claim, worktree and branch stay (A7), the harness record says
   `exited`. A service restart makes running sessions `stale`; `say`
   resumes them.
6. **A refused claim undoes the run's worktree and branch**, which were
   made a moment before and hold nothing; the service never removes
   anything else (A7).
7. **Profiles.** `--profile` → the task's `profile` → `routing.<kind>` →
   `routing.default` → `default_profile` (C10's order) already in stage 1;
   a non-Claude profile is refused until stage 3.
8. **Paths and API.** XDG config and store, `TASKMOOR_HOME` for both; one
   service per store (`flock` on `serve.lock`); `127.0.0.1` only, a random
   token (mode 0600) on every request and a loopback `Host` check; events
   are server-sent with `(epoch, rev)`; a client that missed events takes a
   new snapshot (no replay).
9. **The agent's environment** drops API-key variables (unless
   `api_billing`) and the variables of a Claude Code session the service
   may have been started from (`CLAUDECODE`, `CLAUDE_CODE_SESSION_ID`, its
   IDE port and messaging socket), keeping settings-like `CLAUDE_CODE_*`.
10. **A6 in the CLI.** While a service owns a task (it holds an owner lock
    in the project's state directory), `taskmoor review` refuses it and
    `taskmoor release` hands it back through that service, by the exact
    session the state file names; the service stops the session and removes
    the file only if that claim is still the session's. When no service of
    the CLI's store knows the session, the refusal stands; once the owning
    service has stopped, `taskmoor release ID` works offline as in phase 0.
11. **Real check (2026-11-22, Claude Code 2.1.330, model sonnet, the
    subscription).** On a throwaway repository: Run → the agent asked with
    `AskUserQuestion`, the recommended option marked → answered from the
    inbox → it wrote and committed `hello.txt` on the task branch → the task
    moved to `review`, harness `done`, billing `subscription`. It also showed
    that the person's auto-mode classifier applies inside the session, which
    led to decision 3. After the review fixes a second short session
    (DM-002) checked the hardened flags: `git push` was refused by the deny
    rule before reaching the host, the other commands came through the
    host and ran, the work was committed, the task moved to `review`; the
    restarted service recovered the earlier session.
12. **Review r1 (revust `codex-only`, 25 findings, all confirmed).** Fixed:
    every command, edit and fetch is an ask rule (`--settings`), so allow
    rules in the person's or the project's settings cannot skip the host;
    the deny list covers all A11 commands; Bash writes outside the worktree
    are checked (redirections and write commands, best effort); publish
    tokens and `TASKMOOR_HOME` are removed from the agent's environment;
    an approval shows the whole command; the store lock is taken before
    journal recovery; one owner lock per task in the project's state
    directory makes A6 independent of the CLI's store, and `taskmoor
    release` goes through the owning service; a session touches the state
    file only while its claim (`harness.session` and worktree) is still its
    own; one runner per session at a time, callbacks bound to their runner;
    Shutdown refuses new launches before draining; stop never waits on a
    blocked write; an ask is registered, the session marked waiting, and
    only then announced; the session is recorded before the claim; a refused
    claim removes the worktree and branch only if untouched; the settings'
    API-key check runs on the worktree at every launch; init with a wrong
    billing source or `bypassPermissions` kills the process at once (Claude
    sends `init` only after the first message, so no probe turn is used);
    task ids are looked up within the working directory's project and are
    refused when ambiguous; `feed --follow` handles a reset; `--json` works
    on every service command.
    **Review r2 (27 findings, all confirmed), fixed:** the deny rules and
    the host's classifier cover `kubectl set image` and other cluster
    changes, with global options before the subcommand; a git write naming
    the main checkout (a merge into main from there) is refused, not asked;
    `Read` is an ask rule too (secret files); the person's sandbox cannot
    auto-approve Bash past the host (`autoAllowBashIfSandboxed: false`);
    numbered redirections (`2> f`) and Bash writes to manifests and secret
    files are checked like file-tool writes; release removes the state
    file only after checking, under the project lock, that the claim is the
    session's; a launch is reserved before the session is published, and
    Release and Shutdown wait for or refuse an admitted launch; the owner
    lock is taken inside the claim's project lock; a runner keeps its slot
    until its last write, so a resume never starts before the previous
    process has fully ended; completion writes "done" and `review` in one
    update, and a restart finishes a handover a crash interrupted; an ask
    is answerable and visible only once announced; the billing preflight
    reads `CLAUDE_CONFIG_DIR`, checks the worktree before the claim and a
    resume before any state change, and reads settings files bounded and
    regular-only; the feed returns the state before the history; rollback
    keeps a worktree with untracked or ignored files and deletes the
    branch only at its original commit (`update-ref -d` with the expected
    value); the config is saved through a unique temporary file and edited
    under a lock; `answer --json` reports every failure as JSON.
    **Not fixed in stage 1:** the agent runs as the person — its
    subprocesses can use any credential on disk and can read the API token
    and answer the inbox. The person decided (C13) to confine agents with
    Claude's sandbox in stage 2b; the README states the limit until then.

### Stage 2 — web on localhost and the TUI as a client

- The web UI served by `taskmoor serve`: the board with the conveyor's
  columns (Backlog, Ready, Running, **Needs you**, Review, Merged, Done),
  the project switcher, the card with the session feed, Run / Stop / Add an
  instruction / Open the worktree / Continue in terminal, the inbox with
  forms (recommended option first). No framework decision is taken here:
  stage 2 starts by proposing the web stack to the person (a library versus
  plain server-rendered HTML), per the person's rule.
- The browser signs in through a one-time link `taskmoor serve` prints (or
  `taskmoor web` opens); see the implementation decisions below.
- The TUI shows the conveyor's columns when the service runs (A21) and the
  phase-0 board when it does not.

#### Stage 2 — implementation decisions (2026-11-22)

1. **Stack (the person's choice):** Go `net/http` serves an embedded
   `index.html`, `app.css` and a dependency-free `app.js` (`go:embed`); no
   framework, no build step. The page renders from `GET /api/snapshot` and
   follows `GET /api/events`; every text from tasks and agents is set with
   `textContent`, and a Content-Security-Policy allows only the page's own
   scripts and styles.
2. **Browser sign-in (changed after review r1: no cookie).** A one-time
   code (10 minutes, single use) arrives in the link's fragment
   (`http://127.0.0.1:PORT/#code=…`, from the startup log or `taskmoor
   web`), which the browser never sends to a server; the page trades it
   (`POST /api/sign-in`) for a browser token valid 30 days and keeps it in
   `localStorage`, which belongs to this origin — host and port — alone.
   Every request carries it in `Authorization`, the event stream is read
   with `fetch`, so no other local port receives a credential and no page
   of another origin has one to act with (no CSRF surface). The service
   keeps only the token's SHA-256 in `<store>/web-sessions.json` (0600)
   and spends the code only once that is saved, so a restart does not sign
   the browser out. Open the board at the link's `127.0.0.1` address:
   `localhost` is another origin with its own (empty) storage.
3. **Columns** are derived in the client: backlog not ready → Backlog,
   ready → Ready, `in_progress` with the session waiting for the person →
   Needs you, other `in_progress` → Running, then Review, Merged, Done;
   cancelled tasks are not shown.
4. **Run** preselects the routed profile (the snapshot carries each backlog
   task's route, C10); profiles of harnesses a later stage adds are shown
   disabled; a `needs_person` task needs the person's checkbox (A4).
5. **Continue in terminal (C12)** is `POST …/handover`: the runner stops,
   the state file keeps the claim with harness `unknown` and no
   `session`, the service drops its owner lock, and the session becomes
   `handedOver`; the person continues with the shown command and finishes
   with `taskmoor review`. The CLI has `taskmoor continue ID`.
6. **Open the worktree** runs `open DIR` or the configured `editor`
   command with the worktree as its last argument, on the Mac.
7. **Changes outside the service** (merges, tags, the CLI's state writes)
   reach clients through a watcher that polls each project's fingerprint
   every 2 s and publishes a `tasks` event.
8. **Disconnected ≠ Ended (C5).** The page marks itself Disconnected when
   the stream or a snapshot fails, keeps the last view, and retries; any
   message on a new stream, or a snapshot that succeeds, clears it. The
   event stream flushes its headers at once, and Shutdown ends every
   stream so open browsers never hold the service.
9. **TUI as a client (A21).** When the default store's service runs and
   has the project registered, the TUI reads its snapshot and shows the
   conveyor's columns; the local project still gives the header, the task
   bodies and the refresh fingerprint (plus the service's `(epoch, rev)`).
   Actions stay in the web and the CLI.
10. **Checked in a real browser** (agent-browser, the fake Claude): sign in
    by the link, Run, the question in Needs you and the inbox, the answer,
    the task in Review, Say resuming, Continue in terminal, a CLI release
    showing up on the open board, Disconnected while the service was down
    and reconnecting signed in after its restart. The check found and fixed:
    nested children rendered as text, `null` in the panel, the event stream
    holding Shutdown, unflushed stream headers, and a session completing
    while the service stopped being recorded as stopped.
11. **Review r1 (20 findings, all confirmed), fixed:** the cookie went
    (decision 2); a handover holds the session's launch admission from
    start to end, records itself before the claim changes (Open finishes an
    interrupted one) and repeats idempotently; a resume records `pending`
    before its process starts; the watcher announces a project's first
    look and a removed project; feed activities carry a journal number
    (`seq`) and the page merges history and stream by it, reloading the
    open feed after a reconnect or reset; snapshots are requested one at a
    time and an older one never replaces a newer; drafts, choices and focus
    survive redraws; a failed route preselects no profile; responses for a
    card no longer open are dropped and a failed feed load is retried after
    a pause; the TUI picks the service or the local project at every load
    and stays local with `--source worktree`; the stream test checks the
    headers arrive before any event.
12. **Review r2 (25 findings, all confirmed), fixed:** a handover records
    its intent (`handover_pending`) before letting go of the claim and is
    complete only after; concurrent requests are refused while one runs;
    Open finishes a pending one; a resume writes `pending` durably before
    its process starts, or does not start; the journal closes a torn last
    line before appending and numbers from the highest valid number; the
    snapshot takes its position before reading; the watcher keys projects
    by name and path, announces registrations (loadable or not), removals
    and configuration changes; a session belongs to its project's
    registered path; the board keeps its stream on a reset, refreshes at
    most 250 ms after the first event of a burst, reloads the open feed
    until a load started after a gap succeeds, ignores older body
    responses, clears only the instruction it sent, sends a profile only
    when the person picked one, signs in from a link pasted into an open
    tab and shows an error when sign-in fails; a stream ends when its
    browser token expires; the TUI keeps a valid column when the set of
    columns changes; `feed --follow --json` keeps `seq`.

### Stage 2b — the agents in Claude's sandbox (C13)

- Agent profiles run Claude with its sandbox on: Bash writes limited to the
  worktree (and temporary directories), network limited to what the
  profile allows, the service's store and the person's credential files out
  of reach; `autoAllowBashIfSandboxed` stays off, so the host still sees
  every command.
- The real check runs one session that tries to read the API token, write
  outside the worktree and reach a denied host, and shows each refused.
- The other harnesses (stage 3) get the equivalent of their own, or are
  marked unconfined in the UI until they do.

#### Stage 2b — implementation decisions (2026-11-22)

1. **Settings per session.** The `--settings` JSON is built per session:
   the ask rules as before plus `sandbox.enabled`, `failIfUnavailable`,
   `autoAllowBashIfSandboxed = false` and `allowUnsandboxedCommands =
   false`. `--settings` outranks project settings, so a repository cannot
   loosen it.
2. **Writes.** The worktree, the repository's common git directory (a
   commit writes objects, refs and the worktree's index there), temporary
   directories and build caches (`~/Library/Caches`, `~/.cache`,
   `~/go/pkg/mod`, npm, cargo, gradle, maven). Denied inside it: `hooks`,
   `config` and `info` (code git would later run outside the sandbox) and
   `tasks/` (taskmoor's state files: an agent cannot claim, release or
   mark a task itself).
3. **Reads.** Denied: the service store and the configuration directory,
   and the person's credential files (list in README). Everything else
   stays readable, so builds find their toolchains.
4. **Network.** `allowed_domains` in the profile, default
   `DefaultAllowedDomains` (registries, GitHub for reading);
   `strictAllowlist`, so an unknown host is refused without a prompt; an
   empty list allows none.
5. **The host's rules** refuse Read/Write/Edit/Grep/Glob paths and Bash
   commands that name taskmoor's store or configuration: Claude's file
   tools run outside the sandbox.
6. **Unconfined** is a profile choice (`sandbox = false`); the session
   records `confined`, the board and `taskmoor sessions` show
   `unconfined`. Profiles of other harnesses are unconfined until stage 3
   gives them an equivalent.
7. **Real check, round 0** (claude 2.1.330, Sonnet, one session on a probe
   repository): reading the API token and `~/.ssh`, writing to the home
   directory and to the main repository's `.git/hooks` (also after the
   person allowed the command at the host), and `example.com` were
   refused; `proxy.golang.org`, writes in the worktree and `git commit`
   worked. The same probe outside the sandbox read the token and `~/.ssh`
   and wrote to the home directory.
8. **Review r1 (16 findings, all confirmed), fixed:**
   - a confined session loads only taskmoor's settings
     (`--setting-sources=` and `--strict-mcp-config`) with
     `disableAllHooks`: the person's and the repository's settings could
     add hooks, MCP servers, `excludedCommands` or allowed hosts, which run
     or reach outside the sandbox; the agent can write the worktree's
     `.claude/` itself;
   - git writes are narrowed to the objects, the worktree's git directory
     and the task branch's ref, lock and log; every other existing branch,
     the main checkout's `HEAD`, index and `packed-refs`, and the
     worktree's `.git` file are denied by name, so a writable directory
     around the repository (a temporary one) does not reopen them; a new
     branch can still be created;
   - deny rules for taskmoor's files (both kinds of session) and the
     credentials (confined) keep them out of the file tools, also in
     recursive Grep and Glob;
   - protected paths are compared ignoring case and by file identity of
     the path's ancestors; the service makes its paths absolute;
   - the credential list adds git's XDG store and relocated `gh`/`glab`
     directories (`XDG_CONFIG_HOME`, `GH_CONFIG_DIR`);
   - `allowed_domains = []` survives a configuration save;
   - an unconfined session sets `sandbox.enabled = false`, so the person's
     settings cannot turn it on behind the "unconfined" label;
   - a resume reads the profile as it is now, and the session's
     `confined` flag is saved with its `pending` state;
   - a confined profile needs Claude Code ≥ 2.1.330 (the checked
     version), refused at Run otherwise;
   - not fixed, recorded: the sandbox is checked on macOS only (on Linux a
     missing `~/.cache` is not created and stays unwritable); a startup
     failure leaves the claim and worktree for `taskmoor release`.
9. **Real check, after r1** (the same probe repository, which lives in a
   temporary directory, the worst case for the write rules): moving
   `main` with `git update-ref` was refused (the first attempt, before
   branches were denied by name, moved it: the temporary directory's
   allowance covered the repository), touching the worktree's `.git` and
   writing `.git/hooks` were refused, the repository's SessionStart hook
   did not run, the token stayed unreadable, and `git commit` on the task
   branch worked.
10. **Review r2 (17 findings, the last round), fixed:** managed settings
    that turn the sandbox off refuse a confined launch; the executable is
    resolved to one absolute path for the version probe and the launch,
    and the probe has a 15 s deadline; the worktree's `.git` must name the
    git directory the repository registered for it, or the launch is
    refused; repositories on the reftable backend are refused; the main
    checkout's `config.worktree` and the other worktrees' git directories
    are denied by name; the plan is built before the task's state changes
    (Run: before the claim; Say: before `in_progress`); the settings go in
    a file in the store (`sessions/<id>.settings.json`, 0600), not on the
    command line, so many branches do not overflow it; GitLab CLI and
    Claude credential files are denied; literal paths are escaped in
    permission rules; a confined launch skips the billing scan of
    settings it does not load (init still reports the source); the
    assignment tells the agent to read CLAUDE.md, AGENTS.md and
    `.claude/rules/`; the board labels a handed-over session's sandbox as
    the last taskmoor run. Recorded, not fixed: branches created after a
    session started are not denied to it (the deny list is built per
    launch); a rebase that rewrites tracked `.claude/` files fails in the
    sandbox (Claude protects those paths); README gives the recovery for a
    failed start.
11. **Real check, after r2:** a resumed confined run with the settings
    file: moving `main` refused, the token, the worktree's `.git` and
    `.git/hooks` refused, an empty commit on the task branch made, the
    repository's hook not run.

### Stage 3 — Codex and OpenCode, profiles and routing

- ACP client over stdio; the Codex runner through codex-acp with ChatGPT
  auth and `default_mode_request_user_input` (A26, fallback to Plan mode);
  the OpenCode runner through `opencode acp` with form elicitation and the
  permissions of A28; approvals through `session/request_permission`.
- Profiles and routing: task `profile`/`model` → `[routing]` → default
  (C10), `harness` as a preference (A16, including `validate`'s warning),
  the choice shown and changeable at Run, **Restart with a profile** (A24).
- **Keep N busy** (priority then id, re-derived after every claim, never
  `needs_person`, A4).

#### Stage 3 — implementation decisions (2026-11-23)

1. **One runner interface.** Claude's stream-json runner and the ACP
   runner sit behind one interface (send, allow/deny, stop, close); the
   host callbacks, the rules, the inbox, the journal and the board are
   shared. One ACP agent process per session; a session's turn that ends
   closes its input, and Say resumes in a new process, as for Claude.
2. **ACP client.** Protocol version 1 over stdio; the client advertises
   `elicitation.form` and no file system or terminal. A prompt at a time:
   an instruction during a turn is queued and sent after it. Resume uses
   `session/resume` when the agent offers it, else `session/load` (its
   replayed history is not journalled again). The agent's own session id
   is kept as `agent_session`; taskmoor's session id stays the claim's.
3. **Codex** through `npx -y @agentclientprotocol/codex-acp@2.9.0` (the
   person's choice: a pinned version in npm's cache, nothing installed
   globally; `command` names another). `CODEX_CONFIG` enables
   `default_mode_request_user_input` (A26; the Plan-mode fallback is not
   needed: the real check asked in Default mode), `INITIAL_AGENT_MODE =
   workspace-write`, authentication `chat-gpt` (A17). Preflight requires
   `codex login status` to say ChatGPT unless the profile allows API
   billing.
4. **OpenCode** through `opencode acp`; A27: a profile must set
   `api_billing = true`, its sessions show the API badge. A28 through
   `OPENCODE_CONFIG_CONTENT` (`bash`, `external_directory`, `webfetch`:
   ask; edits in the worktree allowed), `OPENCODE_DISABLE_PROJECT_CONFIG`,
   and an empty `XDG_CONFIG_HOME` of the store's (the person's global
   opencode config loaded their MCP servers
   into the first real session; the sign-in lives in the data directory
   and stays).
5. **Permissions and questions.** An ACP permission request is mapped to
   the rules' tools (execute → Bash with a `zsh -lc` wrapper read through,
   edit → Edit, read → Read, fetch → WebFetch, others asked) and answered
   with the agent's allow_once / reject_once option. A form becomes the
   inbox's question: `oneOf` options, the `(Recommended)` label or the
   field's `default` marks the recommended one, codex-acp's note field is
   left to the free-text answer, the answer goes back as the option's
   constant.
6. **Isolation** is recorded per session: `sandbox` (Claude's, 2b),
   `codex` (Codex's workspace-write: writes and network confined, reads
   not), or none; the board and `taskmoor sessions` show it.
7. **Routing (A16)** as written; the `validate` warning reads the
   service's profiles when its configuration exists.
8. **Restart with a profile (A24):** the old session stops and is
   recorded as stopped with `restarted_as`; a new session (`restart_of`)
   of the chosen profile starts in the same worktree with the assignment
   plus a note to read `git log` and `git status` first; the state file's
   harness record (name, session) moves to it under the claim.
9. **Keep N busy (A4):** `keep_busy` per project in the service's
   configuration (0–32); the watcher's tick fills free slots one task at
   a time, re-deriving readiness after every claim; sessions pending,
   working or waiting for the person count as busy; `needs_person` is
   never started; a failed start is skipped for 10 minutes and shown on
   the board.
10. **Real checks** (this Mac, 2026-11-23): Codex (codex-cli 0.171.0,
    ChatGPT): a form question in Default mode with the recommended option
    first, answered in the CLI and on the board, a file written and
    committed on the task branch, a resume through `session/load`, a
    Restart from an OpenCode session to Codex that moved the harness
    record. OpenCode (2.2.5): the session opened, but OpenCode Console
    reported "SignedOut: Reconnect OpenCode Console" and the turn hung
    retrying; stopped from the CLI. The OpenCode question and approval
    path is checked only against the fake agent until the person signs in
    again.
11. **Review r1 (20 findings, all confirmed), fixed:** an ACP request the
    rules cannot judge (Codex's extra sandbox permissions, a network
    destination, any other kind) always goes to the person; a change of
    several files is judged on every path (renames included); a command
    is judged in its working directory (`cd <cwd> &&`); inherited
    `CODEX_CONFIG`, `MODEL_PROVIDER`, `OPENCODE_CONFIG*` are stripped, and
    a Codex `model_provider` other than OpenAI needs API billing; a failed
    opencode configuration directory refuses the launch; the agent's
    output is a pipe of the service's own with `WaitDelay`, so a helper
    holding it cannot hold Stop or shutdown; every ACP setup call has a
    deadline (3 min for the first, which covers npx); Say during a Restart
    or a handover is refused, Say after the last turn ended resumes
    instead of being lost; Continue in terminal waits for the agent's
    session; a task id never names a session a Restart replaced, and a
    successor a crash left without the claim is recorded as stopped;
    "Keep N busy" reads the target again before every start; form fields
    of one title stay apart, typed fields (number, integer, boolean) are
    encoded and checked before the ask is taken, optional and free-text
    fields are answerable, secret-marked answers are not journalled;
    Codex's asynchronous questions interrupt the turn and the answer is
    the next prompt (A26); a task's `model` wins over the profile's, and
    a model the agent refuses is shown as its default. Recorded, not
    fixed: OpenCode's sign-in is not checked before Run; a manual Run can
    take a project one past its N.
12. **Real check after r1:** a new Codex task (AC-003) through question,
    answer and commit on the task branch.
13. **Review r2 (25 findings, the last round), fixed:** Codex's
    command-scoped `additionalPermissions` make the request the person's;
    a rename's destination comes from the tool call's diff, a move
    without it is asked; a command whose working directory (raw, relative
    or as a location) is outside the worktree is asked instead of being
    prefixed with `cd`; fetches are asked (A28); inherited
    `OPENCODE_CONFIG_PROJECT_DISABLE` is stripped and set; Say and Restart
    are ordered by a per-session lock; Restart checks the successor
    (profile, preflight) before stopping the session, marks the
    predecessor before moving the claim, and Open repairs either half of
    an interrupted Restart (an abandoned successor is never the task's
    session); an asynchronous question holds the conversation however the
    turn ends, its answer goes before queued instructions; an automatic
    start is admitted only while "Keep N busy" still has room, checked
    just before the claim; non-finite numbers and values outside a
    field's bounds are refused, required fields must be answered and one
    text cannot answer several questions, all before the ask is taken;
    Codex's own-answer field is a question of its own; secret answers are
    masked in everything the agent says or titles; a model-setup timeout
    ends the session; a turn's completion is recorded before the exit;
    Continue in terminal for Codex and OpenCode waits for the first
    prompt (the agent's own resume needs it); the board survives an
    option-less field; the Run choice shows a task's own model; with API
    billing allowed and a key present, Codex authenticates by key and the
    badge says so; Stop is tested with a helper holding the agent's
    output. Recorded, not fixed: a turn has no deadline once it runs (long
    work is normal); within the worktree a relative output path is read
    against its root.
14. **Real check after r2:** a Codex task (AC-004): its form now has the
    own-answer field as an optional second question, which `taskmoor
    answer ID 2` first refused (fixed: trailing optional questions may be
    left out); after a service restart, Say resumed the thread, Codex asked
    again, the answer ("hello") was written and committed on the task
    branch, and Continue in terminal gave a `codex resume` id whose
    rollout exists in `~/.codex/sessions`.

### Stage 4 — the dispatcher

- Levels (a)/(b)/(c) with the hard stops first (C3, A11, A13, A14), the
  dispatcher profile's short call, citations checked against the files,
  audit records, overrides delivered as instructions; "answered by the
  dispatcher" in the feed.

#### Stage 4 — implementation decisions (2026-11-23)

1. **Questions only.** The dispatcher looks at questions; approvals stay
   with the rules and the person (C3, GLD-049). It runs on its own
   goroutine after the ask opens: the ask is in the inbox meanwhile
   (marked "the dispatcher is looking"), the person may answer first.
   At most two calls at a time.
2. **Hard stops first (A13).** Before any call: a secret, a required
   free-text field, a multi-choice question, or a word of the (c)
   categories in the question, its header or its options (push, publish,
   deploy, release, merge, delete, remove, drop, rm, secret, token,
   password, credential, API key, private key, dependency, install,
   upgrade, licence, migrate, billing, pay) → the person. Codex's
   optional own-answer field is left empty, not a stop (the real check
   found every Codex question carries one).
3. **The call:** the claude `dispatcher_profile` (refused in the config
   for another harness), `claude -p --output-format stream-json --tools ""
   --setting-sources= --strict-mcp-config --no-session-persistence`, an
   empty temporary directory, the prompt on stdin, `DispatchTimeout` 2
   minutes; an `apiKeySource` other than `none` without the profile's API
   billing → the person.
4. **Documents from the main branch** (`git show main:…`): the task
   file, `AGENTS.md`, `CLAUDE.md`, `README.md`, `docs/*.md`,
   `spec/*.md`; 48 KiB per file, 192 KiB in all. The agent writes only its
   branch, so it cannot plant the answer to its own question.
5. **Checks (A13, A14):** JSON with one entry per question; an option
   that exists; (a) the file is one of those given and the quote (12+
   characters, whitespace normalized) is in it; (b) only the recommended
   option, never for a question without one; any doubt → (c).
6. **Records:** one JSON file per decision in `<store>/decisions/` (the
   audit record's fields), published as a `decision` event; the snapshot
   carries the 30 latest; `taskmoor decisions`, `GET /api/decisions`.
7. **Override:** for an (a)/(b) decision, once; delivered with Say (it
   resumes an ended session) as an instruction naming the question, the
   dispatcher's answer and the person's; recorded as `overridden`.
8. **Real check** (2026-11-23, dispatcher Claude Sonnet on the
   subscription, sessions on Codex): with `AGENTS.md` on main saying
   greeting files contain "hello", Codex asked "hi" (recommended) or
   "hello"; the dispatcher answered (a) "hello" citing the line verbatim,
   Codex wrote and committed it; `taskmoor override … hi` sent the
   person's answer, Codex rewrote the file to "hi". Before the fix in 2,
   the same question went to the person ("free text").
9. **Review r1 (16 findings, all confirmed), fixed:** documents are read
   from `refs/heads/<main>` (a tag of that name is not the branch); a
   secret's path or a dependency manifest named in a question is a hard
   stop; an (a) quote must name the chosen option; the chosen option goes
   by number (a label can be a number), the board's override too;
   managed Claude settings with hooks or an `apiKeyHelper` keep the
   dispatcher out; `CLAUDE_CODE_USE_BEDROCK/VERTEX/FOUNDRY` and
   `AWS_BEARER_TOKEN_BEDROCK` are stripped like API keys (every Claude
   process); the record is written before the answer goes, a failed
   write keeps the question the person's, a delivery failure is recorded
   as such; an ACP answer to a request the agent no longer waits for is
   refused before the ask is taken; a call waiting for a slot or under
   way ends when the ask is answered or withdrawn; a secret given earlier
   is masked in later questions (so the dispatcher never gets it);
   level-c records get the person's answer; overrides can be repeated
   and each is recorded before it is sent (undone if sending fails); the
   board lists the 30 latest dispatcher answers, the CLI and the API all.
10. **Review r2 (15 findings, the last round), fixed:** a form's message
    used as its header is masked too, and a session keeps its secret
    answers in memory for every later runner (a service restart forgets
    them); two options that mask to one label stay apart; managed
    settings are read whole (hooks in any file first, then an `env` block
    with a key or a provider switch, then an `apiKeyHelper`), an
    unreadable file or macOS managed preferences keep the dispatcher out;
    a quote worded against something (never, not, avoid, instead…) is the
    person's; paths are split on every character a name does not use
    (`file=go.mod#L12`, `--config=.env`); the record is "pending", not
    overridable, until the answer is in; a call waiting for a slot gives
    up when the ask ends, and none starts after it ended; the ask is
    linked to its level-c record before the record is published; the
    decision list is read from disk once per change; an asynchronous ACP
    answer says the dispatcher gave it; the board keeps Override after a
    first one (each is sent like Say, so mid-turn it waits for the next
    turn; README says so); README says to remove `dispatcher_profile`
    before going back to an older taskmoor. Recorded, not fixed: whether
    an override reached the agent is not confirmed beyond Say.
11. **Real check after r2:** the same question on a new task (DS-003):
    this time the dispatcher left it to the person (level c), giving as
    the reason that the task tells the agent to ask the person; the model
    errs to the person's side, as C3 wants.

### Stage 5 — review and acceptance

- revust `codex-only` on the branch when a session completes, findings sent
  back into the same session, `min(reviews.code, 2)` rounds (A5).
- The acceptance view (diff against the base, the agent's checks, the review
  result); **Accept** with the trial merge and the real merge of A8 and the
  `Task: <ID>` trailer; send back / release (A7, A6). Push stays the
  person's.

#### Stage 5 — implementation decisions (2026-11-23)

1. **When review runs.** After a session's turn completes and the task is
   moved to `review`; one round at a time across the service. A failed
   round counts (as for revust's own limits); the person can still
   Accept or send back.
2. **revust headless.** `revust new` then `revust --task T --run rN
   --profile <review_profile> --no-tui --tasks-dir <store>/reviews
   --workdir <worktree>`: the archives are the service's, an agent cannot
   commit or edit them. The scope names the branch, the merge base with
   main and the task file as the contract; the goal is the task's body.
   Exit 0 clean, 1 findings (`revust view --markdown`, the Active
   findings part, at most 24 KiB), anything else an error. 90 minutes at
   most; Stop, Release, Restart and Continue in terminal stop a round.
3. **Findings back** with Say: the instruction to check each finding,
   fix what is right (with a test where it fits), say why for what is
   not, commit and finish. After the last round's findings the fixes are
   not reviewed again (`unreviewed`), as in the person's own workflow.
4. **Acceptance view:** commits and diff (256 KiB) against the merge
   base of `refs/heads/main` and the branch, `git status` of the worktree
   (uncommitted work is not merged and is shown), the task's Checks, the
   agent's last summary, the review. Ready only in `review`, with the
   claim the session's, no turn and no review running.
5. **Accept (A8)** as specified; the merge commit is `Merge <branch>:
   <title>` with the `Task: <ID>` trailer; the person's checkout must be
   on main (refused otherwise); one merge at a time. After the merge the
   state file is removed (the derivation says `merged`; a kept file
   would be stale at once), the session is `accepted` with the merge
   commit; worktree and branch stay.
7. **Real check** (2026-11-23, sessions on Codex, review by the real
   revust `codex-only`): AT-001 (`reviews.code: 1`): Codex wrote and
   committed `greet.sh`, the review round ran in 1.5 minutes and was
   clean, the board's acceptance view showed it ready, `taskmoor accept`
   made `Merge task/AT-001-greet: Add a greet script` with `Task: AT-001`
   on main's first-parent history and the board moved the task to Done
   (the project's release rule is `merge`). The check found the view
   printing nested lists as text; fixed and seen again on AT-002.
6. **Send back** is Say from the acceptance view, prefixed "Sent back from
   acceptance:". The confirmation on the Mac itself for dangerous
   approvals (A9, A11) is not needed here yet: the board and the CLI are
   local; it comes with remote clients.
8. **Review r1 (23 findings, all confirmed), fixed:** Accept merges the
   commit the person saw (the view's `tip`; a branch that moved is
   refused) and the trial and the merge use the same two commits; nothing
   runs while the person's checkout is in a merge, rebase, cherry-pick or
   revert (their merge is never aborted: only one this call started is);
   a path the merge writes that exists untracked or ignored in the
   checkout refuses it (git's ort strategy overwrites ignored files
   whatever `--no-overwrite-ignore` says); `--commit --no-squash --no-log`
   keep the person's `merge.log` and `mergeOptions` from skipping the
   commit or burying the trailer; branch commits already in main are
   "nothing to merge"; after the merge HEAD, the parents and the trailer
   (read by `git interpret-trailers`) are checked before the claim goes;
   the session is held during Accept (no Say, Release, Restart or
   handover); a task the derivation already calls merged is not accepted
   again; the trial runs through gitx (an inherited `GIT_DIR` is
   dropped); a conflict without named paths is reported as one; a late
   exit does not overwrite `accepted`; Accept waits while a review is due
   or its findings are on their way; a review waiting for its slot is
   cancellable and re-checks the session before it runs; a service
   restart turns a running round into an error; Restart carries the
   review budget; the review names the branch's commit, not the
   worktree's HEAD; the review record is copied on write; a stop sends
   revust SIGTERM (it cleans up its providers), then kills after 30 s;
   a clean round with fewer sources than run is marked partial; the diff
   is read with a limit; the board copes with no commits; the CLI says
   when the diff is cut; this guide no longer promises check results.
9. **Found after r1 by the tests:** Shutdown did not end review rounds,
   which went on starting revust after the service had stopped (and a
   process forked at that moment held the store lock for a while); now
   Shutdown cancels and joins them, and none starts while it stops.
   Real check after r1: AT-003 on Codex, a clean revust round of the
   named commit, `taskmoor accept` showing the commit it merges, the
   merge with `Task: AT-003`.
10. **Review r2 (24 findings, the last round), fixed — Accept no longer
    runs `git merge`.** The checks on this Mac showed that both `git
    merge` (ort, whatever `--no-overwrite-ignore` says) and `git
    read-tree -m -u` overwrite ignored files. Accept now: makes the merge
    commit itself from the trial's tree (`git commit-tree`, parents main's
    checked tip and the task commit, the exact message: no strategy,
    merge option, `merge.log` or hook of the person's config is
    involved); checks every path the merge writes, its parent
    directories and the contents of a directory it replaces against the
    list of files tracked at main's tip (no pathspecs), and refuses on
    anything untracked or ignored in the way; moves the person's index
    and files with `git read-tree -m -u` (it refuses when their changes
    are in the way); moves main with `git update-ref` only from the
    checked tip (compare and swap; the files go back if that fails); never
    aborts anything; reads the trailer exactly. Also: Release is refused
    while Accept holds the session; Shutdown waits for an Accept under way
    and Accept refuses while it stops; the acceptance view uses one commit
    of the branch throughout and names the project's main branch; Accept
    needs the commit the person saw (`--tip` in the CLI, the view's tip on
    the board); an accepted session stays accepted whatever a late
    callback writes; the review decision is made before Accept can look
    (a completion holds Accept until its review is started or not); a
    clean review of an older commit does not cover a newer one while the
    budget lasts; Restart checks the successor before it touches the
    review, then lets a running round end and counts it before the budget
    moves; Open gives a completed session the review it is owed and marks
    findings saved but not delivered; a round never clears the next
    round's cancellation; `revust view` runs under the round's context,
    bounded, and an unreadable report is an error, never clean; a
    reported partial outcome is marked; each round has an archive name of
    its own; Stop stops a review that runs without an agent; session
    records are written one at a time through temporary files of their
    own (two writers could leave one's tail in the other's file).
    Recorded, not fixed: a merge commit made by `commit-tree` runs no
    commit hooks and is signed only if the person's config signs
    `commit-tree` commits.
11. **Real check after r2:** AT-004 on Codex. Codex stalled once on its
    model call (no events for minutes; Accept refused meanwhile, "the
    agent is still working"); after a service restart and Say it
    finished, the real revust round was clean, `taskmoor accept AT-004
    --tip a4227417` made the merge (parents main's tip and a4227417,
    `Task: AT-004`), the file appeared in the checkout, an untracked
    `notes.local` there stayed untouched, the task became done.

### Stage 6 — guards and power

- Billing badges for all three harnesses (C2, A17, A27, A30); profiles that
  would bill an API key start only when allowed.
- Usage guard: remaining plan usage per harness, read by taskmoor itself
  (F11's sources); the threshold that blocks new starts is proposed to the
  person in this stage.
- The idle-sleep assertion while any agent runs (A19).

#### Stage 6 — implementation decisions (2026-11-23)

1. **Billing badges** were in place for the three harnesses from stages
   1–3 (Claude's `apiKeySource`, Codex's ChatGPT sign-in or key, OpenCode
   always API, A27) with the refusals of A17 and A30; this stage adds the
   provider switches to what is stripped (stage 4) and nothing else.
2. **Reading the plans** (the person's choice: as Guild reads them):
   Claude through `security find-generic-password -s "Claude
   Code-credentials" -w`, the token in memory only, `GET
   https://api.anthropic.com/api/oauth/usage` with `anthropic-beta:
   oauth-2025-04-20`, read forgivingly (`limits`, else the older
   `five_hour`/`seven_day`); a 429 waits its `Retry-After` (15 minutes
   without one). Codex through `codex app-server`: `initialize`,
   `initialized`, `account/rateLimits/read`, `primary` and `secondary`.
   Only harnesses with a subscription profile are read; every 5 minutes;
   a failed read keeps the last figures, marked.
3. **The guard** (the person's choice): the lowest window's share left
   against `usage_threshold` (default 10): below it, automatic starts
   wait (Keep N busy's starts, a review round — revust's `codex-only`
   runs on Codex's plan, a postponed round is not counted — and the
   dispatcher, whose question goes to the person); manual Run, a Say
   that resumes and Restart are refused unless `anyway` (`--anyway`; the
   board's "… anyway" button). API-billed profiles and unknown plans are
   not guarded.
4. **Power (A19):** one `caffeinate -i -w <service pid>` while at least
   one agent runs (it ends with the service whatever happens); the
   snapshot's `agents_running`; the board's note "agents running — keep
   the Mac awake (lid open or on power)".
5. **Real check** (2026-11-23): `taskmoor usage` read Claude (75 percent
   left: session 89, week 75, week · Fable 100) and Codex (26 percent
   left in the week window); a running session held an assertion on
   behalf of the service's process, released a second after Stop.
6. **Review r1 (18 findings), fixed:** the guard sits on the resume path
   of every Say (the person's, the review's findings as an automatic
   start, an override, Accept's send-back), not only on the HTTP Say;
   review rounds and the dispatcher check it again after waiting for a
   slot; a postponed round runs when the plan is back above the
   threshold; rounds are guarded on the plan of revust's
   `review_profile` (codex-only by default, read even with no codex task
   profile); windows past their reset no longer count; a profile that
   allows API billing is exempt only when a key is there (without one it
   runs on the plan); the token's request follows no redirect and uses
   HTTP/1.1 only (Go's HTTP/2 debug log prints headers); `Retry-After`
   as a date is read; the first reading is always published; Codex's
   app-server runs in its own process group with a pipe of the service's
   own and a deadline that holds; review rounds and the dispatcher's
   call hold the Mac awake too; a `caffeinate` that fails or ends is
   shown (and tried again by the next agent); Claude's runner reads its
   output through a pipe of its own, like the ACP runner; the board
   offers Restart and Send back anyway, and Say anyway keeps a newer
   draft. Not changed: a new CLI against an older running service fails
   on the new request fields (restart the service after an upgrade, as
   README says).
7. **Review r2 (15 findings, the last round), fixed:** an old
   `caffeinate`'s end no longer clears the assertion that replaced it
   (a generation); after every reading the service goes on with what a
   low plan held back (postponed rounds, findings that could not resume
   the session), not only on a crossing; a round neither starts nor runs
   for a session that went on meanwhile; the plan is checked once more
   right before revust's providers and the dispatcher's call start (a
   postponed round is not counted); review profiles other than
   codex-only and claude-only are guarded on both plans (revust resolves
   their runners); Claude routes that bill without a key in the
   environment (an `apiKeyHelper` in its settings, Bedrock, Vertex,
   Foundry) count as API billing for the guard and are not read as a
   plan; Override and Accept's send-back are the person's confirmation
   (they resume anyway), and the send-back is recorded once delivered;
   Restart anyway restarts the profile that was confirmed; Keep N busy
   does not mark tasks failed when the plan is low (the pass stops);
   Send back anyway keeps a newer draft; README names
   `usage_threshold` in the rollback note. Recorded, not fixed: a board
   redraw drops a "… anyway" button that was showing (the action is
   refused again and offers it again).

### Stage 7 — Telegram

- The bot (C6, A9, A12, A20): outbound polling, questions with option
  buttons and a link to the form on `localhost`, only the person's account,
  deadlines re-notify; dangerous approvals wait for a confirmation on the
  Mac (A9), whose mechanism (Touch ID / password through macOS) this stage
  settles.

#### Stage 7 — implementation decisions (2026-11-23)

1. **Dangerous approvals (A9), the person's choice:** answered on the Mac
   — the board on 127.0.0.1 and the CLI. Telegram never carries a button
   for an approval, only the notice and the link. v1 checks no physical
   presence: whoever holds the board's sign-in (made by `taskmoor web` on
   the Mac) or the API token can approve, a browser on the person's own
   ssh forward to that address included — the laptop path A9 describes is
   not built in v1 and its forward cannot be told apart from the Mac. So
   A9's Touch ID / password step on the Mac is deferred, by the person's
   choice, to v2 together with the remote web's passkey (C7, A10).
2. **The bot's secret:** `taskmoor telegram setup` reads the token from
   stdin (never an argument; from a terminal without echo), the service
   checks it with `getMe` and keeps it in `<store>/telegram.json` (0600);
   the setup endpoint takes only the API token's credential (the CLI), not
   a browser's. Every request carrying it (the CLI's to the service
   included) follows no redirect, Telegram's use HTTP/1.1; errors are
   cleaned of it. Claude sessions cannot read the store (2b); Codex's
   sandbox leaves reads unconfined and OpenCode has none, so their
   sessions can read the token, as they can the person's other files
   (a recorded limit of v1).
3. **Pairing:** a one-time code (10 minutes, spent on use) sent as
   `/start CODE` in a private chat binds the person's user id and chat;
   every update from anyone else, and every other message, is ignored
   without an answer. The settings are held in memory under one lock with
   a generation: setup and off replace them, drop the code and cancel the
   poll in flight, so a batch an older poll brings back answers, pairs
   and saves nothing.
4. **Delivery:** long polling (`getUpdates`, 25 s) from the Mac; the
   next offset comes from the ids returned, and one saved over 6 days ago
   is dropped (Telegram may restart its ids after a week). The service
   subscribes to its own event bus and then sends what is already open
   (again after a gap, and after a pairing); a failed send is tried again
   a minute later, and a send's error shows in `telegram status` until
   one succeeds. Whether the dispatcher looks at a question is decided as
   it opens: if so, it is sent once the dispatcher leaves it to the
   person ("for you"), and a dispatcher turned off meanwhile leaves it
   too. Simple questions (A12: one question — codex-acp's own-answer
   field beside it aside, marked as such by the ACP runner — single
   choice, at most 8 options, no previews, not secret, and short enough
   that the question and every choice show in full: 1000 bytes, 250 per
   choice) get inline buttons, numbered as the full choices
   in the text, the recommended first with ★; the rest get their
   questions (a secret one only named) and the board's link, which is
   never cut off. An approval's message carries nothing of the request (a
   command can hold a secret): only that one waits, and the link. Option
   previews are kept and shown on the board. Pressing a button answers
   through the same path as the board ("answered in Telegram" in the
   feed), off the poll, at most 4 in flight; a resolved ask edits its
   message to "✓ answered", also one answered while its message was on
   the way.
5. **Reminders (A20):** an ask still open `TelegramRemind` (30 minutes)
   after its last message is sent again (the older message points to it);
   an `ask_reminder` event goes to the clients every 30 minutes an ask
   waits for the person, on its own clock, whether Telegram got it or
   not. Nothing else changes.
6. **Review r1** (15 findings, all fixed): poll/setup/off generations;
   approvals without detail; retries, late pairing and the catch-up order;
   the dispatcher's disposition fixed at open; previews board-only and on
   the board; answers off the poll; offsets from the returned ids; full
   numbered choices; no redirects from the CLI; setup without echo;
   the Codex/OpenCode read limit recorded; board-only links never cut;
   status events on setup and off; the reminder test synchronised.
7. **Review r2** (17 findings, all fixed; the last round): a pairing is a
   generation of its own, and each generation's context cancels its
   requests (polls, sends, a callback's answer) and its delivery records
   start empty, so a new pairing gets what is open at once; a press is
   taken only if its account is still heard as the ask is claimed
   (`answerIf`, under the service's lock, then the bot's); the pairing's
   note and catch-up run off the poll; the offset is kept in memory when
   its file cannot be written and the failure shows; edits that fail are
   tried again on the minute's pass, which also closes messages of asks
   resolved while an event was missed; links use the port listened on;
   buttons only when every choice fits; only the marked own-answer field
   is exempt; `telegram setup` reads the token before finding the
   service and sends it only after the listener proves, by an HMAC of a
   nonce with the API token, that it is the service; Ctrl+C at the hidden
   prompt restores the terminal; serve joins the bot's goroutines before
   it returns; the dispatcher test holds the dispatcher's reply.

### Stage 8 — ideas and intake (v2)

- Raw ideas, grilling through forms, intake on `intake/<id>` and Approve
  into main (C8, C9, A22). Chosen by the person as the first part of v2
  (2026-11-24), built without a design discussion at the person's word;
  the calls below are the agent's.

#### Stage 8 — implementation decisions (2026-11-24)

1. **An idea** is a record in `<store>/ideas/<id>.json` (id `idea-` and 8
   hex digits): project, title (the first line when none is given), the
   raw text (at most 64 KB), state `new → grilling → proposed → approved`,
   or `dropped`; the grilling's session, branch, worktree and base; the
   task ids it adds; the merge commit. The transcript is the session's
   journal in the store (C8): nothing of it reaches git but the tasks.
2. **Grilling is a session** of the existing runners, not a new engine:
   Grill makes `intake/<id>` from main's tip in `<repo-dir>-<id>` (A23's
   rule), routes the profile as a `design` task would (tasks.toml
   `routing.design`, `routing.default`, the default profile; `--profile`
   wins), runs the same preflight, usage guard and sandbox, and sends a
   first message that tells the agent to read the repository, grill the
   idea through AskUserQuestion (one decision per question, the
   recommended option first, each option's trade-off), write the tasks in
   the repository's format with a Context summary (A22), validate them
   with taskmoor and commit only `tasks/`. A grilling session has `idea`
   set and no task: it holds no claim and writes no state file; Release,
   Restart and Accept refuse it; it is not one of "keep N busy"'s N; its
   resume and Continue in terminal work as a task session's.
3. **Its questions are the person's:** the dispatcher never sees them
   (the ask opens without a dispatch); they reach the inbox and Telegram
   with the idea's id where a task id stands.
4. **Proposed:** when the grilling's turn completes with commits on its
   branch, the idea is `proposed` with the task ids it adds (read from the
   worktree against main). Saying more to the session goes on grilling.
5. **Approve (C9)** lands the intake at the commit the person looked at
   on the local main as **one commit**: its tree is that commit's tree,
   its parent main's tip, its message `Intake <id>` with `Tasks:` and
   `Idea:` lines (the idea's id, never its raw title or text, C8; no
   `Task:` trailer: it merges no task). The intake's own history stays on
   its branch, so a file it committed and removed again never becomes
   reachable from main. It goes through Accept's checks (the same
   functions: the checkout on main and between operations, no untracked
   or ignored file overwritten, `read-tree -m -u`, main moved by compare
   and swap from the main it reviewed). Approve refuses while the grilling
   runs, with nothing committed, with no task added, with uncommitted
   changes in the worktree, with any change but a regular, not deleted
   task file directly in `tasks/` or `tasks/tasks.toml` (listed from the
   two trees with `diff-tree -z`, submodules included, no diff
   configuration), and while the project, read at that commit as main
   would be (`LoadAt`), does not validate: nothing on disk counts. The
   tasks it adds are those of the commit not in the merge base. A main
   that has moved since the branch was made is not landed on: Approve
   sends the session "merge main, give your tasks ids that do not clash,
   validate, commit" and reports it; the person looks again after. The
   intended commit is written to the idea before main moves, so an
   Approve a crash cuts off is finished (or cleared) at the next start,
   review, Approve or Drop. The worktree and branch stay (A7); push stays
   the person's.
6. **Drop** stops the grilling and marks the idea dropped; the branch and
   worktree stay. Approve and Drop hold the grilling's session (no Say or
   resume starts meanwhile, one under way refuses them); every change of
   an idea's record runs from copy to store under one lock, so a late
   completion cannot undo a Drop. Grill writes its intent (session,
   branch, worktree, base) before it makes them; a Grill cut off is
   cleared by the next one if what it made is untouched. Review and
   Approve check that the project is still the checkout the idea was
   added to and that the worktree belongs to its repository.
7. **Clients:** the CLI's `taskmoor idea add|list|show|grill|review|
   approve|drop`; the board's Ideas column (new, grilling and proposed
   ideas, **+ New idea**), whose panel holds the idea, Grill with a
   profile choice, the review and Approve, Drop, and the grilling's
   session and feed. The TUI does not show ideas yet.
8. **Review r1** (17 findings, all fixed): validation of the commit, not
   the worktree; the path check from the trees; one commit on main
   instead of a merge (history, raw title); main pinned to the reviewed
   one; added tasks against the merge base, so an id main took is sent
   back; the approval intent and its reconciliation; Grill's intent and
   leftovers; Approve and Drop holding the session; serialized idea
   writes; the project and repository checks; the CLI sends the full
   reviewed commit and says when the diff is cut; the inbox opens an
   idea's question on the idea.
9. **Review r2** (13 findings, all fixed; the last round): the review
   validates the very commit Approve would land (the intake's tree on
   main's tip, made with `commit-tree` and left unreferenced), so a
   reference to an intake-only commit fails; with main moved, validation
   errors do not hold back the send-back (they are checked again after);
   Approve needs the main the person reviewed as well as the tip; the
   diff is git's own (`--no-ext-diff --no-textconv`), and a diff or status
   that cannot be read withholds readiness; an approval intent is
   reconciled only under the ideas' lock and kept when git cannot answer,
   and an Approve cut off between `read-tree` and `update-ref` (the index
   at the approved tree, main at the old tip) is finished by moving main;
   a runner being stopped counts as running until it has exited; a
   Grill's leftovers are removed only if the worktree is this repository's,
   on the intake branch, at the base and clean; a grilling whose first
   launch failed with nothing made starts again on Grill; `taskmoor
   continue` of a grilling prints the idea's next steps.

### Stage 9 — the web from outside (v2)

- The VPS path of C5 with C7's step-up and A10. The person's choices
  (2026-11-24): Pocket ID with oauth2-proxy as the outer passkey sign-in
  (A10's off-the-shelf component), go-webauthn/webauthn for the service's
  own passkeys, and the VPS files prepared by taskmoor and applied by the
  person (nothing deployed by taskmoor).

#### Stage 9 — implementation decisions (2026-11-24)

1. **A second listener.** With `remote_host` (the board's domain) and
   `remote_port` set, `taskmoor serve` also listens on
   `127.0.0.1:<remote_port>`, which only the reverse tunnel reaches (the
   VPS's Caddy proxies to the tunnel's end on its own loopback). Requests
   there are remote by where they arrive, never by a header: that
   listener has no sign-in links, no API token, accepts only the Host
   `remote_host` (with `:443` or its port), and serves the same API and
   board. The Mac's own listener is unchanged and knows no device token.
2. **Devices.** `taskmoor device pair` (the Mac's CLI or board) makes a
   one-time code (10 characters, 10 minutes, one at a time, all codes
   void after 10 wrong tries); on the device the board asks for it and a
   name, then registers a passkey for `remote_host` (user verification
   required, no attestation asked) with go-webauthn. The device gets a
   random token, kept in the store only as a SHA-256 hash with the
   passkey's credential (`<store>/devices.json`, 0600), and sends it as
   `Authorization: Device <token>`. `taskmoor device list`, `revoke ID`
   (its token, passkey, pending confirmations and open event stream end).
3. **Step-up (C7, A10, A11).** From a device, Accept, Approve and an
   answer that allows an agent's request get 428 naming the action
   (`accept:<session>`, `approve:<idea>`, `allow:<ask>`); the board asks
   the device's passkey for it (a fresh WebAuthn assertion, user
   verification required, the signature counter checked) and retries
   with a one-use grant for that action and device (2 minutes). A deny,
   a question's answer, Say, Run and the rest need none. Remote requests
   never reach opening Finder or an editor on the Mac, Telegram's
   setup, pairing or devices.
4. **The outer sign-in** is not the service's: Caddy's `forward_auth` to
   oauth2-proxy, which signs in through Pocket ID (passkey-only OIDC);
   oauth2-proxy and Pocket ID listen on the VPS's loopback, Caddy on the
   host's network to reach the tunnel. Only the person has a Pocket ID
   account.
5. **The files.** `taskmoor remote files --vps USER@HOST --auth-domain D
   [--vps-port N] [--key PATH] [--out DIR]` writes, by default under
   `<store>/remote`: the Mac's launchd agent (`ssh -N -R
   127.0.0.1:<vps-port>:127.0.0.1:<remote_port>`, BatchMode,
   ExitOnForwardFailure, keep-alives, KeepAlive, its own key), the VPS's
   `authorized_keys` line (`restrict,port-forwarding,permitlisten=…`,
   no shell), `docker-compose.yml` with pinned images (Caddy 2.11.8,
   Pocket ID v2.19.1, oauth2-proxy v7.15.5), the Caddyfile, `.env`
   with a new Pocket ID encryption key and oauth2-proxy cookie secret
   (0600; written once, `--force` for new ones) and a README of the
   steps.
6. **Checked** with a software passkey in the tests (real P-256
   signatures through go-webauthn) and in Chrome with a WebAuthn virtual
   authenticator on `localhost` (pairing, a step-up that allowed an
   agent's request, a revoked device back at the pairing form).
7. **Review r1** (10 findings, all fixed): an answer's body is one JSON
   object, read by the step-up guard as the handler reads it (anything
   after it is refused); Accept's grant names the session itself (a task
   id or project resolves once and the request goes on with the session
   id); step-up actions are only `accept|approve|allow:<id>`, at most 8
   under way per device and 64 in all; a revocation is written before it
   takes effect, so a failed one can be retried; the tunnel account gets
   its own sshd policy (`AllowTcpForwarding remote`, `PermitListen`,
   `PermitOpen none`, `GatewayPorts no`, no stream-local, agent, X11 or
   TTY) beside the key's options, Caddy's admin API is off, and the
   launchd agent checks the VPS's host key strictly (the README has the
   first, verified connection); Pocket ID is reachable only from
   `AUTH_OPEN_TO` (the person's own address) until its first account —
   its administrator — exists; oauth2-proxy accepts Pocket ID's
   unverified email of that account (nothing decides by email); the CLI
   writes each file through a new temporary file of its mode; the Mac's
   board has a Devices panel (pairing code, list, revoke).
8. **Review r2** (19 findings, all fixed; the last round): a remote
   request is read whole before its device and grant are checked, so a
   slow upload cannot outlive a revocation or a grant; an answer must be a
   JSON object (`null` is refused); a pairing finishes only while its code
   is still the one issued, and no ceremony outlives its code; a passkey's
   counter is written before a grant is given; a remote event stream ends
   after 10 minutes and the board reconnects through the VPS's sign-in;
   an API request whose VPS sign-in expired gets 401 with
   `X-Taskmoor-Login: required` from Caddy and the board sends the page to
   the sign-in (the device stays paired); Telegram's links point to the
   board from outside when it is on; the tunnel's VPS port may not be
   Pocket ID's or oauth2-proxy's; oauth2-proxy runs on the host network
   on 127.0.0.1, Pocket ID's loopback publishing needs Docker Engine 28;
   the README installs the sshd policy where sshd reads it and checks it
   with `sshd -T` for the tunnel account (an earlier Match or a missing
   Include shows), and the account's connections time out
   (`ClientAliveInterval 30`) so a stale one lets go of the listener;
   `.env` is written last and explains that its secrets belong to one
   deployment (recreate, do not restart, after changing it). The Mac's
   Devices button, broken in r2's commit by an edit of the agent's, was
   fixed and checked in the browser.

### Stage 10 — the board as a phone app (Later: "PWA first")

- The person's choices (2026-11-24): the PWA first of the Later list, with
  push notifications, implemented on Go's standard library (no new
  dependency).

#### Stage 10 — implementation decisions (2026-11-24)

1. **The app.** `manifest.webmanifest` (standalone, the icon drawn as
   three board columns, 180/192/512 px), `sw.js` and the iOS meta tags.
   The service worker caches only the board's own files, network first
   (a new version shows at once), never `/api/` or the VPS's sign-in; the
   board keeps its last snapshot in the device's storage (cleared on
   sign-out) and shows it, marked Disconnected, when the service cannot
   be reached; it remembers whether it is the board from outside.
2. **The phone's layout** (under 900 px): one column, the top bar wraps,
   the panel takes the whole screen within the safe areas, controls at
   least 36 px, inputs at 16 px (iOS does not zoom).
3. **Web Push, own code.** RFC 8291's aes128gcm encryption (ECDH P-256,
   HKDF-SHA-256, AES-128-GCM, one record) checked byte for byte against
   the RFC's example, and RFC 8292's VAPID (an ES256 JWT for the
   endpoint's origin, 12 hours, `sub` the board's https origin). The
   VAPID key is made once in `<store>/vapid.json` (0600).
4. **Subscriptions** belong to paired devices only (the board from
   outside): `GET /api/push/key`, `POST /api/push/subscribe`,
   `/unsubscribe`, kept in `<store>/push.json`, at most 4 per device,
   only to a known push service's https host (Apple's, Google's,
   Mozilla's, Microsoft's) so a device cannot make the Mac post
   elsewhere; a subscription answered 404 or 410 is forgotten; revoking
   the device forgets its subscriptions.
5. **What is pushed:** an ask that waits for the person (as Telegram is
   told: after the dispatcher, once per ask) and each reminder (A20). The
   title is project · task; a question's body is its first question
   (not a secret one), an approval's only "An approval waits for you";
   a tag per ask (a newer message replaces it), `Urgency: high`, TTL six
   hours (a message a push service accepted before a revocation may still
   arrive within it). A click opens the inbox (`/#inbox`).
6. **Checked:** the RFC vector, a round trip, the VAPID signature; a fake
   push service over TLS that decrypts what it gets (the approval's
   message, the reminder, a 410 that forgets the subscription); in
   Chrome at iPhone size: the layout, the service worker active, an
   approval allowed with the device's passkey, the board offline. A real
   subscription was not made from the headless browser (it would post to
   Google's push service): that is the person's check on the phone.
7. **Review r1** (12 findings, all fixed): every POST to a push service
   and every new subscription is admitted under a gate that revocation
   holds while it removes the device, and checks that the device is still
   paired (a revoked device's leftover subscription is dropped, never
   used); the event follower only queues (a sender posts), so a slow
   push service cannot drop the follower from the bus, and a queue that
   overflowed, or a new subscription to the bus, reconciles the open asks
   not announced yet; a push refused for now (429, 5xx, a network error)
   is tried again for that subscription after its Retry-After (1 s to
   10 min, 4 tries in all); any refused credential — an API call or the
   event stream — clears the board in memory and its offline copy, and a
   response for a credential gone meanwhile is dropped; the board from
   outside has a **sign out** that clears the offline copy before the
   VPS's sign-out; a copy too big for the device's storage is removed
   rather than left old; the service worker shows its copy when the VPS
   answers but the Mac does not (5xx); the phone layout keeps to the side
   safe areas in landscape.
8. **Review r2** (16 findings, all fixed; the last round): an ask is
   marked announced only once its message is queued (the queue is
   bounded, 1024, and an overflow reconciles); the reconciliation also
   sends a reminder whose event was missed (from the clients' reminder
   clock); each message of an ask has a version, and a retry is dropped
   once the ask is answered or a newer message of it was queued; the
   revocation's wording says what a push service already holds may still
   arrive (TTL six hours); a revoked device's subscriptions left by a
   failed write are cleared every minute; a subscription's key must be a
   point of P-256; the board's sign out lands on `/signed-out`, a page
   Caddy serves outside the sign-in (no automatic sign-in again); every
   sign-out, refusal or VPS refusal forgets the board in all tabs of the
   origin (a session generation in storage, the storage event) and drops
   responses of an older generation; a 401 signs out only the credential
   it was sent with; a copy the storage refuses removes the older one;
   the service worker installs only with the page, script and styles
   cached (the rest best effort); number and password fields get the
   phone sizes too.

### What each stage needs from the person

| Stage | Decision or permission |
| --- | --- |
| 1 | Agreement to this split; permission for the real-check Claude session. |
| 2 | The web stack (proposed with options at the start of the stage). |
| 3 | Permission for real-check Codex sessions; OpenCode runs bill an API key (A27), so its real check needs explicit permission or stays on fixtures. |
| 4 | The dispatcher profile. |
| 5 | Nothing new (revust `codex-only` is already the route). |
| 6 | The usage threshold. |
| 7 | The bot token and the person's Telegram account id. |
| 8 | Nothing new; a real grilling runs on the subscription. |
| 9 | The outer sign-in, the WebAuthn library, who deploys; at the end the VPS, its domains and the deployment, which the person does. |
| 10 | Push in or out, own code or a library; the real check on the phone after stage 9's deployment. |

## Relation to Guild

Since C12 none of this is needed for v1; the table records where the
later Guild client and Guild's own backlog meet the conveyor.

| Guild | Relation |
| --- | --- |
| GLD-017 — task dashboard and launcher design | **Answered**: the person chose a separate tool; taskmoor is it, and this document designs its launcher and merge. |
| GLD-018 — Guild Chat surface over ACP | **Overlaps**: taskmoor is an ACP client for background agents; Guild's chat surface is a separate, interactive client. Neither depends on the other. |
| GLD-049 — attributed agent messages, a parent may answer delegated technical questions, never the person's decisions | **Consistent**: the dispatcher is such a parent; every answer is attributed ("answered by the dispatcher"); the person's approvals are never answered. |
| GLD-010…016 — local session server (remote deferred there) | **Overlaps** in kind, not in scope: taskmoor's service owns background agent sessions, Guild's server owns terminals. taskmoor's remote access is for its web UI only. |
| GLD-042 — model choice for launched sessions | **Overlaps** with C10 (found while writing; not named by the person). |
| GLD-031 — instruction queue and prompt delivery | **Overlaps** with "Add an instruction" (found while writing). |
| GLD-032 — handing a task over to another session | **Overlaps** with "Continue in terminal" (found while writing). |
| `AGENTS.md`: non-visible workers need human question, tool-approval and resume channels | The conveyor provides these three channels (inbox/Telegram, approvals, resume). |
| `guild agent list --json`, OSC 7501 | Still the plan's harness source for sessions that run in Guild (e.g. after "Continue in terminal"). |
| `crates/desktop/src/usage.rs` | The reference for reading plan usage (F11). |

## Verify before code

Run on 2026-11-22 on the person's Mac, each as one short session on the
subscription (no API key set; `apiKeyHelper` absent), in throwaway
directories under the session scratchpad. Nothing that bills an API key was
run.

| Check | Result |
| --- | --- |
| Claude billing | ✅ `apiKeySource: "none"` in `system/init` (F12). `claude /status` is interactive, so `apiKeySource` is the runner's check instead. |
| Codex billing | ✅ `codex login status`: "Logged in using ChatGPT"; probes ran on it. |
| OpenCode billing | ✅ checked, not run: the only provider is OpenCode Console by API key (F18), so a run would bill the key; opencode profiles need "API billing allowed" (A27). |
| Read `agentclientprotocol/codex-acp` | ✅ alive, read at `e3a679f19345` (F17). |
| Codex request-user-input outside Plan mode | ✅ absent by default; present with `default_mode_request_user_input`; the turn waits for the answer (F15) → A26. |
| `opencode acp` forms | ✅ from the v2.2.5 source (F18), not live (API billing). |
| ACP session ids = resume ids | ✅ Codex: verified live (F16). OpenCode: from source (F18). |
| Claude `defer` → `tool_deferred` → `--resume` + `updatedInput` | ✅ works, but only with the stdio permission host (F13, F14) → A15. |
| Telegram from the Mac | ✅ reachable (F19). |

## Open questions

The questions this design raised are closed by **agent decisions A1–A30
(2026-11-22)**, taken on the person's instruction to decide them without
asking, and by the checks in [Verify before code](#verify-before-code). They
are not the person's decisions: any of them is overridden by the person's
word, and the spec review checks them first. Nothing is left open.

### Agent decisions (2026-11-22)

- **A1 — harness status writes (conflict with `plan.md`).** `plan.md` said
  taskmoor writes only the initial harness record at `claim`. Decided: the
  CLI and the TUI stay readers; `taskmoor serve`, being the launcher, keeps
  the harness record current for the sessions it runs. `plan.md`'s "Harness
  status" section now says so in one sentence.
- **A2 — how Guild attaches to a background session.** *Superseded for v1
  by C12: see "Without Guild" under [v1 in stages](#v1-in-stages-without-guild);
  the text below is kept for the later Guild client.* v1: "Open the
  worktree in Guild" and "Continue in terminal". Continue hands the session
  over: the service stops its runner, marks the session "continued in
  terminal", and status then comes from the plan's Guild source (OSC 7501,
  `guild agent list`). A feed view inside Guild is Guild as a service client,
  with the Guild UI buttons (Later). The Guild commands used are whatever
  Guild's CLI offers at implementation time.
- **A3 — `harness.session`.** While the service runs a session it writes the
  harness's own conversation id. On handover it writes Guild's session id if
  Guild reports one, otherwise removes `session`, so the plan matches by
  worktree.
- **A4 — `needs_person` tasks.** "Keep N busy" never starts them. The
  person's explicit Run on such a task is the `--person` statement.
- **A5 — review rounds.** Rounds = `min(reviews.code, 2)`. `reviews.code: 0`
  skips revust; the task goes straight to acceptance.
- **A6 — CLI writes to a task the service runs (review r1).** The service
  writes `writer: "taskmoor serve <version>"`. While the service is running
  and owns the task, `taskmoor review` and `release` from the CLI refuse with
  "owned by taskmoor serve; stop or release it there" — there is no `--force`
  past a live owner. A release through the service is a handover: it stops
  the runner first, so no late write from the old session follows, then
  removes the state file. Only when the service is not running may the CLI
  `release --force` a file it wrote.
- **A7 — stop, failure, decline.** The task keeps its claim (`in_progress`
  or `review`), worktree and branch; the session is stopped/`error`. The
  card offers: "send back with an instruction" (same session), "restart with
  a profile" (A24), or "release" (A6 handover, then the plan's `release`).
  Worktrees and branches are never removed by the service.
- **A8 — merge on Accept and Approve (review r1).** Before touching the
  person's checkout the service runs a trial merge with
  `git merge-tree --write-tree <main> <branch>`, which writes nothing to any
  worktree. A conflict there sends the task back to its session with the
  instruction to merge main into its branch; the main checkout is untouched,
  so a later Accept starts clean. Only a clean trial is merged for real:
  `git merge --no-ff --no-overwrite-ignore --no-autostash` with the
  `Task: <ID>` trailer, in the checkout that has the main branch. These two
  flags keep git's refusal on the person's touching uncommitted edits (C9)
  and stop it from overwriting an ignored local file, whatever the person's
  git config. If that merge still fails, the service runs `git merge --abort`
  and reports it. The same applies to Approve of an `intake/<id>` branch.
  Stale state files are left to the plan's stale rule.
- **A9 — Telegram and remote clients in v1 (review r1).** v1 Telegram
  answers simple questions with buttons; its link opens the form on
  `localhost`. Every other question can be answered from any client,
  including the laptop over `ssh -J`. **Dangerous approvals (A11) need a
  confirmation on the Mac itself** — a Touch ID / password prompt the service
  raises through macOS (mechanism to be settled in implementation), which a
  connection forwarded over ssh cannot pass. In v2 the remote web adds C7's
  repeat passkey (A10) for the same approvals.
- **A10 — who checks the repeat passkey (v2).** The service: each paired
  device registers a passkey for the VPS domain at pairing, and the service
  verifies the step-up for each dangerous approval, whichever path the
  request came by. The outer login before the tunnel is an off-the-shelf
  passkey-capable forward-auth component for Caddy, chosen at the start of v2
  by checking its passkey support; no own login server.
- **A11 — dangerous approvals (review r1).** Deletions outside the task's
  branch, anything touching secrets, and the person's Accept/Approve merges.
  **Push, publish and deploy are not approvals at all:** an agent session is
  never allowed to run them — the runner rejects such a request without asking
  (Claude: a deny rule/hook; Codex and opencode: reject the approval) and the
  agent is told that push stays the person's. A *question* about pushing or
  publishing still goes to the person as a form (C3); only the command is
  never approved. A merge into main by the agent is rejected the same way; merges happen only through Accept/Approve. The
  service itself never pushes.
- **A12 — simple Telegram questions.** One question, single select, no
  previews, not secret. Everything else gets only the link.
- **A13 — how the dispatcher decides.** Two steps. Fixed rules first:
  push/publish/deploy commands are rejected outright (A11); tool and
  approval kinds, commands (`git merge` into main, `rm` outside the
  worktree), paths of secrets and dependency manifests → level (c). Then one
  short call through the service's **dispatcher profile** (one subscription
  profile set in the service config) with the question, the task file and the
  project's guidance files; it returns `{level, answer, citation, reason}`. A
  reply that is not well-formed, or a citation that does not exist in the
  named file, makes it level (c).
- **A14 — no recommended option.** Such a question is never answered at
  level (b): (a) with a citation, or the person.
- **A15 — Claude question wiring (from F13, F14).** The runner always starts
  `claude -p` with `--permission-prompt-tool stdio` and acts as the
  permission host; without it there is no `AskUserQuestion`. Questions go
  through a `PreToolUse` hook that `defer`s, so no process is held while the
  person thinks; the answer is given by `claude -p --resume <id>` with a hook
  returning `allow` and `updatedInput.answers`. A turn with several tool calls
  (where `defer` does not apply) is answered through the host while the
  process lives. The assignment tells the agent to ask only from the main
  conversation, since subagents cannot ask (F3).
- **A16 — `harness` versus `profile` (review r1).** An explicit `profile`
  wins. Otherwise `harness` other than `any` is a preference, as the format
  says: among the configured profiles of that harness the routed one is used
  if it matches, else the first such profile; if there is none, the normal
  route is used. `validate` warns when `profile` and `harness` disagree.
- **A17 — other API keys.** The billing guard treats `OPENAI_API_KEY` and
  any provider key a profile names as API billing for the badge and for the
  "not started unless allowed" rule. Only C2's Anthropic variables are
  stripped; other keys are left in place, because a custom-model profile
  needs them. For Codex the runner chooses codex-acp's ChatGPT auth method
  (F17), so a stray `OPENAI_API_KEY` does not switch billing.
- **A18 — store engine.** Plain files, no database: a JSONL journal per
  session, one JSON file per audit record batch and per pending ask, TOML for
  the service config, all written by temp file + rename. No new dependency.
- **A19 — power and the lid.** The service holds the idle-sleep assertion
  while any agent runs; the board says "agents running —
  keep the Mac awake (lid open or on power)".
- **A20 — pending-ask deadline.** A deadline only re-notifies (Telegram and
  connected clients); the ask stays in the inbox and the session stays
  `awaitingInput`; nothing is answered on the person's behalf.
- **A21 — phase-0 TUI columns.** Phase 0 keeps the plan's five columns. The
  TUI, when `taskmoor serve` runs, becomes its client and shows the
  conveyor's columns; without the service it is the phase-0 board.
- **A22 — where the agreement lives in git (C8).** In the task file: Goal,
  Acceptance and Limits carry what was agreed, Context carries a short
  summary of the grilling. Transcripts stay in the service store.
- **A23 — worktree location.** A sibling directory of the repository,
  `<repo-dir>-<id-lowercase>` (e.g. `taskmoor-tm-012`), branch
  `task/<ID>-<slug>`.
- **A24 — model choice under "keep N busy" and restarts (review r1).**
  Those launches use the routing result without a dialog; the card shows the
  profile/model. "Restart with a profile" is an operation on the existing
  claim, not a new Run: the service stops the runner, keeps the state file,
  worktree, branch and their changes, starts a new session of the chosen
  profile in the same worktree with the task assignment plus a note on the
  work already there, and rewrites the harness record (`name`, `session`).
- **A25 — when the format additions land.** With the phase-0 spec move:
  `profile`, `model` and `[routing]` go into `spec/` (schema and validation
  only; phase 0 does not act on them), so the format stays `format = 1`.

- **A26 — Codex questions (from F15, F17; review r1).** The Codex runner
  starts the app-server under codex-acp with `default_mode_request_user_input`
  enabled (the service's Codex config for its sessions; exact wiring to be
  settled in implementation) and advertises form elicitation. A Codex
  question holds the live process until it is answered. If a Codex update
  removes or breaks the flag (the probe in Verify before code fails), the
  runner falls back to the `collaboration_mode` option: the first turn in
  Plan, where Codex asks, then Default for the work.
  `request_user_input_async` questions (they do not stop the turn) go to the
  dispatcher at once: (a)/(b) are answered while the turn runs; a level (c)
  one makes the service interrupt the turn (`session/cancel`), put the
  question in the inbox, and send the person's answer as the next prompt.
- **A27 — OpenCode on this Mac.** Every opencode profile is API-billed today
  (F18), so each one is created with "API billing allowed" set explicitly by
  the person in the profile, and its cards show the "API key" badge. Adding a
  subscription provider later (ChatGPT, Copilot) changes only the profile.
- **A28 — questions opencode cancels silently (F18; review r1).** The
  conveyor cannot see them, so opencode sessions must not be able to act on
  a level (c) matter unasked: the runner sets opencode's permissions for
  shell commands and for edits outside the worktree to `ask`, so every such
  action arrives as `session/request_permission`, which is never filtered and
  goes through the dispatcher's hard stops. What remains is work inside the
  task's branch, which reaches main only through the person's Accept, where
  the diff shows it. The assignment tells the agent to ask with plain options
  and never about secrets; its stated assumptions show in the feed.
- **A29 — claims are serialized per project (review r1).** The service and
  the CLI re-check readiness and publish a claim under one per-project lock,
  `flock` on `<git-common-dir>/tasks/state/.lock`. Without it two claims of
  different ids with overlapping `touches` could both succeed (the `link`
  publish guards one id only). `docs/plan.md`'s `claim` says so.
- **A30 — Claude billing before the first turn (review r1).** Stripping
  variables does not disable an `apiKeyHelper` set in Claude's settings. The
  runner reads the settings files Claude would load for the worktree (user,
  project, local, managed) and refuses to start a profile without API
  billing when any of them sets `apiKeyHelper`; it then waits for
  `system/init` and stops the session before sending the first prompt
  unless `apiKeySource` is `none` (whether `init` arrives before the first
  prompt in `--input-format stream-json` is to be confirmed in
  implementation; if it does not, the first prompt is a no-op probe turn).
