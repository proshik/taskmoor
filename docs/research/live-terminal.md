# Watching and joining a running session's terminal (TM-002)

Research for the last open item of Later in `docs/conveyor.md` (C11):
"attaching to a background session's live terminal". The person wants to
watch a running session as it works, and type into it without taking it
over. This document says what each harness offers for that today, gives
three designs, and ends with a recommendation and the questions only the
person can answer. Nothing here is decided; no code changed.

Checked on this Mac on 2026-11-25 with the installed versions:

| Harness | Version | How the service runs it today |
| --- | --- | --- |
| Claude Code | `claude --version` → `2.1.331 (Claude Code)` | `claude -p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio …` (`internal/serve/claude.go:80`) |
| Codex | `codex --version` → `codex-cli 0.172.0` | `npx -y @agentclientprotocol/codex-acp@2.9.0` over `codex app-server`, ACP over stdio (conveyor.md, stage 3, decision 3) |
| OpenCode | `opencode --version` → `opencode v2.4.1` | `opencode acp`, ACP over stdio (stage 3, decision 4) |

Stages 2b and 3 were checked on Claude Code 2.1.330, codex-cli 0.171.0 and
opencode 2.2.5 (conveyor.md, "Facts the design rests on"). OpenCode has
moved two minor versions since then. That matters below: 2.4.1 has a
shared background server and steering, which the stage 3 notes do not
mention.

How the sources are marked:

- **help**: the installed binary's `--help` output, run on this Mac.
- **docs**: a page of the harness's documentation, read on 2026-11-25. The
  URL is given.
- **schema**: `codex app-server generate-json-schema --out <tmp dir>`, the
  protocol the installed app-server describes itself.
- **code**: source code read on this Mac. That means taskmoor's own source,
  the pinned codex-acp 2.9.0 in npm's cache
  (`~/.npm/_npx/1b16a7d619b2aa9c/node_modules/@agentclientprotocol/codex-acp/`),
  or the strings of the installed `opencode` binary.

## What the service already does

These facts set the baseline. Every design below is measured against them.

- **The feed is the watch view.** The runners turn harness events into
  journal activities: thoughts are not shown; actions, responses, errors
  and results are (`internal/serve/claude.go:452`). The web and
  `taskmoor feed ID --follow` stream them (stage 1, stage 2).
- **Claude already takes input mid-turn.** `Say` to a running Claude
  session writes a stream-json user message to the live process's stdin
  (`service.go:948`, `claude.go:331`). Claude's docs say what happens to
  it: "A message you send during tool calls reaches Claude after those tool
  calls finish. Claude then answers it and your earlier message in one
  response. If the turn ends first, Claude starts a new turn for it."
  (docs: <https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode>).
- **Codex and OpenCode do not take input mid-turn yet.** The ACP runner
  queues an instruction "when … no turn is in flight (ACP has one prompt at
  a time)" and sends it after the turn ends (`acprunner.go:396`, stage 3,
  decision 2). OpenCode's ACP docs confirm the limit: "A session runs one
  prompt at a time. OpenCode returns an error for a second `session/prompt`
  while the first is active." (docs: <https://opencode.ai/docs/acp/>).
- **Taking over is one-way.** Continue in terminal (`POST …/handover`,
  `taskmoor continue ID`) stops the runner and drops the claim's session.
  The person then runs `claude --resume <id>`, `codex resume <id>` or
  `opencode --session <id>`, and finishes with `taskmoor review` (stage 2,
  decision 5). The service has no way to take the session back.
- **What a new path must not weaken** (AGENTS.md, "What must not
  weaken"):
  - every tool call is judged by the host's rules (`policy.go`);
  - Claude's sandbox comes from the per-session `--settings` file;
  - dangerous approvals from remote devices need a passkey (C7);
  - the dispatcher never answers the person's approvals.

## What each harness offers

### Claude Code 2.1.331

| Capability | What exists | Source |
| --- | --- | --- |
| Input mid-turn in the mode the service uses | Yes. A stream-json user message sent during a turn is read after the current tool calls finish. If the turn has already ended, it starts a new turn. Docs: "Messages pushed during a response can also be given a `priority`" to control when Claude reads them. | docs: <https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode> |
| Interrupt mid-turn | Yes: an `interrupt` control request. The service already sends it on Stop (`claude.go:359`). `system/init` advertises `interrupt_receipt_v1` and `interrupt_cancel_queued_v1`. | code; docs: <https://code.claude.com/docs/en/headless> |
| Finer-grained watching | `--include-partial-messages` ("Include partial message chunks as they arrive (only works with --print and --output-format=stream-json)"). `--forward-subagent-text` sends subagents' text as messages. | help |
| Background sessions another terminal can join | `claude --bg` starts a session under a supervisor process. `claude attach <id>` opens it in a terminal ("The session keeps running either way"). `claude logs <id>` prints "recent terminal output". `claude agents --json` lists the sessions. But `--bg` "Can't be combined with `-p`/`--print`", so it is the interactive TUI, which has no `--permission-prompt-tool` host. | help (`claude --help`, `claude attach --help`, `claude logs --help`); docs: <https://code.claude.com/docs/en/agent-view>, <https://code.claude.com/docs/en/cli-reference> |
| `--resume` of a running session | "Resuming one that is still running opens that session in this terminal through `claude attach`". Running two writers on one session (two terminals, or two `-p` runs) loses messages: "both write to one transcript. When you resume that session later, messages from one of them can be missing." | docs: <https://code.claude.com/docs/en/cli-reference>, <https://code.claude.com/docs/en/sessions> |
| A PTY mode | Not for `-p`. The supervisor runs background sessions' terminals; the docs call only `--exec` jobs "PTY-backed". Any PTY the service wanted would be its own: it would run the interactive `claude` in a pseudo-terminal itself. | docs: <https://code.claude.com/docs/en/agent-view> |
| Remote Control | `--remote-control` makes claude.ai/code and the Claude app "a window into that local session". Messages and the transcript pass through and are stored on Anthropic's servers ("the session transcript … is stored on Anthropic servers"). The docs do not mention it for `-p`. | help; docs: <https://code.claude.com/docs/en/remote-control> |
| Reading the transcript live | `~/.claude/projects/<project>/<session-id>.jsonl` is "saved continuously", but its format is internal: "scripts that parse these files directly can break on any release". | docs: <https://code.claude.com/docs/en/sessions> |

What this means for Claude:

- The only supported channel for a session the service drives is the
  stream-json pipe. The service already reads it and already writes
  mid-turn input to it.
- "Joining" the same live process from a terminal does not exist for `-p`.
- `--bg` and `attach` give a joinable session, but only in interactive mode.
  That mode loses the stdio permission host, which the rules, the inbox and
  the dispatcher are built on.

### Codex 0.172.0 (through codex-acp 2.9.0)

| Capability | What exists | Source |
| --- | --- | --- |
| Input mid-turn | `turn/steer` "append[s] user input to the currently in-flight turn without creating a new turn". Its required parameters are `threadId`, `input` and `expectedTurnId` ("The request fails when it does not match the currently active turn"). The docs do not mark it experimental. The `steer` feature shows as `removed  true`: built in, no longer a switch. | docs: <https://learn.chatgpt.com/docs/app-server>; schema (`v2/TurnSteerParams.json`); help (`codex features list`) |
| …through ACP | codex-acp 2.9.0 has the extension request `_session/steering`. Its README says: "Mid-turn input through the `_session/steering` extension, with an opt-in host-owned fallback when no turn can accept it." The adapter injects the prompt into the running turn and answers `{outcome: "injected"}`. With `_meta.steering.idleBehavior = "promptRequired"` and no turn running, it answers `{outcome: "promptRequired", reason: "noRunningTurn"}` and leaves the prompt to the client. Without that flag it starts a new turn itself. | code: codex-acp `README.md:19`, `dist/index.js:49179`, `:52912`, `:54780–54830` |
| Several clients on one running thread | `codex app-server --listen unix://PATH` (or `ws://IP:PORT`, "experimental and unsupported"). The schema's `thread/resume` says: "If thread_id identifies a running thread, app-server rejoins that thread". There are also `thread/unsubscribe` and `thread/loaded/list`. | help (`codex app-server --help`); schema (`v2/ThreadResumeParams.json`); docs: <https://learn.chatgpt.com/docs/app-server> |
| The TUI as such a client | `codex --remote unix://PATH` and `codex resume --remote …` "Connect the TUI to a remote app server endpoint". `codex app-server daemon start` and `codex app-server proxy --sock` exist, and the `daemon_auto_start` feature is `stable true`. `codex --no-daemon` says the TUI otherwise uses "the shared background server". | help |
| Queueing a message from outside | `codex queue --thread <id> --message <text> [--resume]` "Queue[s] a message for an existing session" on the local daemon or a `--remote` server. | help; docs: <https://learn.chatgpt.com/docs/developer-commands?surface=cli> |
| Which client answers approvals | Not documented for ordinary approvals. The one rule found is for human-command approvals: "Only capable clients receive or answer these requests". `thread/resume` takes `approvalsReviewer`: "Override where approval requests are routed for review on this thread and subsequent turns." | docs: <https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>; schema |
| Remote control | `codex remote-control start|stop|pair` runs the daemon "with remote control enabled" over "a secure relay layer" for "authorized ChatGPT devices". The docs do not say who runs the relay. | help; docs: <https://learn.chatgpt.com/docs/remote-connections> |

What this means for Codex:

- Mid-turn input is ready today through the adapter the service already
  runs: one extension request.
- Joining from the real Codex TUI is possible in principle: an app-server
  on a socket, and `codex resume --remote` to rejoin the running thread.
- But codex-acp spawns its own app-server over stdio. The service would have
  to own the app-server and speak its protocol, not ACP.
- How approvals are split between two attached clients is not documented.

### OpenCode 2.4.1

| Capability | What exists | Source |
| --- | --- | --- |
| Input mid-turn through ACP | `opencode acp` registers `_session/steering` with `{sessionId, prompt}`. Its strings include `cli.acp.turn.steer`, `promptRequired` and `noRunningTurn`, the same shape as codex-acp's. Not exercised; the outcome values are read from the binary. | code: strings of `~/.opencode/bin/opencode` (v2.4.1) |
| Input mid-turn in its own clients | TUI: "While OpenCode is working, **Enter** steers the active session; press **Ctrl+X**, then **Enter** to queue the prompt for later." HTTP: `POST /api/session/{id}/prompt` ("Returns once the message is queued"), plus `/api/session/{id}/inbox`, whose delivery mode can be changed ("Steering wakes session execution"). | docs: <https://opencode.ai/docs/tui/>, <https://opencode.ai/docs/server/> |
| A shared server clients join | "By default, OpenCode discovers or starts one shared background server for your user account. Every local OpenCode client connects to that server, which owns sessions." `opencode --server <url>`, `--session <id>`, `--standalone`, `opencode service start|stop|status`, `opencode mini --session <id> --replay`. Events: `GET /api/event`, "Volatile by contract: a slow consumer overflows and fails the stream". Interrupt: `POST /api/session/{id}/interrupt`. Permission and form replies are per request (`…/permission/{requestID}/reply`, `…/form/{formID}/reply`, which answers 409 once settled). | help (`opencode --help`, `opencode service --help`, `opencode mini --help`); docs: <https://opencode.ai/docs/cli/>, <https://opencode.ai/docs/server/> |
| …but not for ACP sessions | "`opencode acp` starts a private OpenCode server for its own use. It doesn't connect to the shared background service". A TUI on the shared server therefore does not see the service's sessions live. | docs: <https://opencode.ai/docs/acp/> |
| Which client answers permissions | Not documented. The 409 on a settled form suggests the first answer wins (an inference). | docs: <https://opencode.ai/docs/server/> |
| Remote pairing | `opencode pair [--remote]`: "Pair through the OpenTunnel remote address … (exposes the server to the internet)". | help |

What this means for OpenCode:

- Mid-turn input through the ACP the service already speaks: the same
  extension request as Codex.
- A joinable live session would mean running sessions on an `opencode
  serve` server, not `opencode acp`: a new runner over its HTTP API.
- Joining also needs a server the person's TUI can reach, so the agent can
  reach it too (see design B).

### Summary

| | Mid-turn input on today's channel | A second client on the running session | Native PTY |
| --- | --- | --- | --- |
| Claude | yes, already used | only interactive `--bg` + `attach`, which loses the permission host | no |
| Codex | yes: `_session/steering` → `turn/steer` | yes: app-server on a socket + `codex resume --remote`; approval routing undocumented | no |
| OpenCode | yes: `_session/steering` | yes: `opencode serve` + `--server --session`; not for `opencode acp` sessions | no |

## Designs

### A — the feed plus a mid-turn input channel

Keep today's headless sessions. Make "watching" a richer feed and "typing
into it" a real mid-turn message for all three harnesses.

**Changes in the service:**

- *ACP runner.* `send` during a turn sends `_session/steering` with
  `idleBehavior: "promptRequired"`, instead of queueing.
  - `injected`: the instruction is journalled as delivered.
  - `promptRequired`: it goes out as the next `session/prompt`, as today.
  - The extension is missing or returns an error: queue as today.
  - The client learns support from the agent's capabilities or from a
    method-not-found error; both adapters register the method by name.
- *Claude runner.* No change to delivery. Optionally pass the message
  `priority` the SDK documents, so the person chooses between "now" and
  "after this turn".
- *Watching.* Optionally add `--include-partial-messages` for Claude and
  ACP's message chunks for the others, so text streams as it is written.
  Optionally show commands' output in the feed, not only the command line.
  Partial text is a lot of events: it would go to the live stream only,
  not into the journal.
- *Web and CLI.* While a turn runs, the instruction box says when the text
  will arrive ("after the current step" or "after this turn"). The feed
  marks the `prompt` activity injected or queued. `taskmoor say` gains a
  flag for "after this turn".
- *Recording.* A decision in the stage that does it.

**The rules:** unchanged. An instruction is text to the agent. Every tool
call it leads to still comes through the host and `policy.go`.

**The sandbox:** unchanged. No new process, file or socket.

**Risks:**

- `_session/steering` is an extension (leading underscore), not ACP v1.
  Both adapters have it today; either could rename it. The queue fallback
  keeps the feature working if it disappears.
- Claude: a message written just before `result` may start a new turn
  ("If the turn ends first, Claude starts a new turn for it"). But
  `sessionHost.result` closes the input and completes the session
  (`host.go:132–137`), so Claude might answer after the task has moved to
  Review. This is not checked; the stage needs a probe. The likely fix is
  to hold completion while a sent message has not been answered.
- It is not a terminal: the person sees what the harness reports, not its
  screen.

### B — the harness's own live session, joined from a terminal

Run each session so that the harness's own TUI can attach to the running
process alongside the service. The service stays a client.

**Changes in the service:**

- *Codex.* The service starts `codex app-server --listen unix://<store>/codex/<session>.sock`
  itself and speaks the app-server protocol directly, in place of codex-acp:
  a new runner (thread, turn, approval and `requestUserInput` messages).
  The board shows `codex resume --remote unix://… <thread>`, which rejoins
  the running thread.
- *OpenCode.* The service runs one `opencode serve` (its own, not the
  person's shared service) with the A28 configuration, and drives sessions
  over the HTTP API (prompt, events, permission and form replies,
  interrupt): a new runner. The board shows `opencode --server <url>
  --session <id>`.
- *Claude.* Nothing equivalent keeps the permission host. The only way to
  join is `claude --bg` + `claude attach`, which means interactive mode. The
  service could then judge tool calls only through a `PreToolUse` or
  `PermissionRequest` hook that calls the service. But confined sessions
  run with `disableAllHooks` (stage 2b, decision 8), and the hook command
  runs outside the sandbox. So for Claude, B falls back to C below.
- *The board.* It tells the person which client answered an approval.

**The rules:** at risk. Approvals reach every attached client that can
answer them. In the person's TUI, an approval is answered by a keypress
that never passes `policy.go` or the hard stops, and the push/publish
refusals (A11) rest on the service seeing the request first. For Codex,
`approvalsReviewer` might keep approvals with the service; the
documentation does not say so. A probe would have to show that the TUI
cannot answer them. For OpenCode, the first reply probably wins (an
inference).

**The sandbox:** a new hole to close. Whoever reaches the socket or port
can drive the agent and answer its approvals, including the agent itself.
- *Codex:* its workspace-write sandbox confines writes and the network, but
  not reads (stage 3, decision 6), and whether it blocks connecting to a
  unix socket is not checked.
- *OpenCode:* its server listens on TCP, and OpenCode sessions are
  unconfined today.

The socket needs a path and a token the agent cannot reach. Proving that
is new work for each harness.

**Risks:**

- Two new runners on interfaces the docs call experimental (app-server) or
  change between minor versions (OpenCode 2.2 → 2.4 added a whole
  background-service model).
- Losing ACP's single code path from stage 3.
- No Claude support.
- Remote use (from the VPS or the phone) does not fit: the TUI is local.
  Exposing the socket would bypass C5's device tokens and C7's passkeys.

### C — hand the session to a terminal and back

Make today's one-way Continue in terminal a round trip. The person takes
the session into their terminal, types, and gives it back. The service
resumes it headless with the same id.

**Changes in the service:**

- *Handover.* As today (stage 2, decision 5), but the shown command carries
  taskmoor's own flags, so a confined Claude session stays confined in the
  terminal: `claude --resume <id> --settings <store>/sessions/<id>.settings.json
  --setting-sources= --strict-mcp-config --permission-mode default`, or a
  `taskmoor continue ID --exec` that runs it. Codex and OpenCode get the
  same treatment with their configuration (`CODEX_CONFIG`,
  `OPENCODE_CONFIG_CONTENT`, the empty `XDG_CONFIG_HOME`).
- *Hand back.* New: `POST …/takeback`, `taskmoor take ID`, and a board
  button.
  - The service checks that the terminal's process has ended: for Claude,
    `claude agents --json` and no writer on the transcript; for the
    others, the person confirms.
  - It takes the claim back (`harness.session` restored under the project
    lock, the owner lock retaken).
  - It resumes with `--resume`, `session/resume` or `session/load`, and
    adds a note to the assignment to read `git log` and `git status`, as
    Restart does (stage 3, decision 8).
- *The feed.* While the session is in the terminal, the board shows "in
  the terminal" and no feed. Reading Claude's transcript JSONL to fill the
  feed is possible, but the format "can break on any release", so it stays
  out of scope.

**The rules:** in the terminal the person answers the harness's own
prompts. That is the person approving, as today. On hand back the rules
apply again. The deny list (`--disallowedTools`) and the settings' ask
rules can travel with the command, so push and publish stay refused in the
terminal too for Claude.

**The sandbox:** for Claude, the same settings file keeps the same
confinement in the terminal, which today's command (`claude --resume <id>`)
drops. Codex and OpenCode keep what their configuration gives.

**Risks:**

- Two writers if the person hands back while the terminal still runs:
  "messages from one of them can be missing" (docs, sessions). The take
  back must refuse while the terminal's process lives.
- The settings file is a path into the store, which is denied to agents.
  The command must be run by the person, not by an agent.
- It is taking over, not typing alongside. The person cannot watch the
  feed and type at once.

### Not proposed: a PTY per session that the service owns and the web streams

This is the design the task names first. It is described for completeness.

The service would run the interactive TUIs (`claude`, `codex`, `opencode`)
in pseudo-terminals and stream them to the web through a terminal emulator.
That fails on several counts:

- **The structured channel goes away.** Questions, approvals and costs
  appear on a screen, not in events. The dispatcher, the inbox, Telegram,
  the rules and the billing check (`system/init.apiKeySource`) would all
  have to scrape it, or rely on hooks that confined Claude sessions turn
  off.
- **Keystrokes bypass the approval guards.** A keystroke from any web
  client can approve anything. That defeats C7's passkey for dangerous
  approvals from remote devices.
- **It needs a dependency.** The web needs a terminal emulator (for
  example xterm.js). That is an npm dependency against the board's "no
  framework, no npm" rule (AGENTS.md), or a hand-written emulator.

Its only gain over A is the harness's real screen.

## Recommendation

**Do A, then C. Leave B until a probe shows approvals stay with the service.**

- **A** gives "type into it without taking it over" for all three
  harnesses:
  - small changes, on the channels the service already owns;
  - no change to the rules, the sandbox or remote access;
  - for Claude it mostly already exists;
  - for Codex and OpenCode it is one extension request with today's queue
    as the fallback.

  The stage should begin with two probes on real sessions: an instruction
  sent mid-turn to Codex and to OpenCode (the `injected` answer, and the
  model acting on it), and a Claude message racing the turn's end.
- **C** fixes the two real faults of today's Continue in terminal: the
  sandbox is dropped in the terminal, and there is no way back. It needs
  no new harness interface.
- **B** is the only design that shows the harness's real TUI live beside
  the service. But it costs two new runners, has no Claude support, and
  opens a socket the agent must be kept out of. Its approval routing is
  undocumented. It is worth doing only if the feed from A is not enough
  to watch by, and only after probing:
  - whether `codex resume --remote` can answer an approval the service
    should judge;
  - whether `approvalsReviewer` prevents that;
  - whether a sandboxed agent can connect to the socket.

## Questions only the person can answer

1. **Is the feed enough to watch by, or do you want the harness's own
   screen?** A, optionally with streamed text and command output, keeps
   the feed. Only B (Codex and OpenCode) or the PTY design shows the real
   TUI.
2. **When you type during a turn, should it go in now or after the turn?**
   The default could be "after the current step" (steer), with a choice of
   "after this turn" (queue), or the reverse.
3. **May remote devices (the phone, the VPS) send mid-turn instructions?**
   Today `say` works remotely. A steered instruction can change what the
   agent does next, but every tool call still passes the rules. Is the
   device token enough, or should a passkey be required, as for dangerous
   approvals?
4. **In a joined terminal (B) or a handed-over one (C), may your keypress
   approve what the service's rules would have asked you about, or
   refused?** C keeps Claude's deny list in the terminal. B's routing is
   unknown until probed.
5. **Should hand-back (C) resume the session at once, or wait for a
   prompt from you?**
6. **Is Claude's or Codex's remote control (through Anthropic's or
   OpenAI's relay) acceptable for watching from the phone, outside
   taskmoor?** It would put the session's transcript on their servers, as
   C6 accepted for Telegram's question text. It also bypasses the
   service's rules for that session.
