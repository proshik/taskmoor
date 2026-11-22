# taskmoor — agent conveyor

Status: **proposal; decisions answered by the person 2026-11-19…22; spec
review done 2026-11-22.** The review (revust `codex-only`, architecture lens,
one round) raised 11 findings; all were checked and fixed, marked
"review r1" in the decisions they changed.
Agent decisions A1–A30 (2026-11-22) close the open questions, and the
[Verify before code](#verify-before-code) checks were run on 2026-11-22;
the person may override any agent decision.

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
3. **Acceptance view:** the diff against the base, the checks the agent ran
   and their results, the review result. **Accept** merges the branch into
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
| At the Mac | `http://localhost:<port>` |
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

## Relation to Guild

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
- **A2 — how Guild attaches to a background session.** v1: "Open the
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
