# AGENTS.md

How to work in the taskmoor repository. The README says what taskmoor is
and how to use it; this file says how to change it.

## The project

- Go 1.26, one module (`github.com/proshik/taskmoor`), one binary
  (`cmd/taskmoor`). The package map is in the README's **Layout**.
- `spec/` is the task format (`format = 1`): `README.md` and the JSON
  schemas, embedded in the binary. A change to what a task file or a state
  file may contain is a change to the spec and the schemas first, and
  `internal/validate` with it; a change existing files would fail needs a
  format bump, which is the maintainer's decision.
- `docs/plan.md` (the format and the CLI) and `docs/conveyor.md` (the
  service) hold the design and every decision taken, stage by stage. Read
  the parts your change touches before changing it; when it settles
  something new, record it there (as a numbered implementation decision
  of the stage, with why) and keep the README in step.
- taskmoor's own work is planned in `tasks/` (its own format, prefix
  `TM`): read the task you are given with `taskmoor show ID` and work to
  its Acceptance, Checks and Limits.
- The web board is plain JavaScript and CSS embedded from
  `internal/serve/web/` (no build step, no framework, no npm).

## Checks

```sh
just check                          # gofmt, go vet, golangci-lint when installed, go test
go test -race -count=1 ./...        # what CI runs (Linux and macOS)
go test -race -run TestName ./internal/serve/
node --check internal/serve/web/app.js internal/serve/web/sw.js
```

`internal/serve` takes about six minutes with `-race`. Run the package you
changed while working and everything before you finish.

## Tests

- Tests run against real git repositories made by `internal/testrepo`, and
  against the fake agents in `internal/serve/testdata` (`fakeclaude`,
  `fakeacp`, `fakerevust`), built by the tests. A test never reaches a
  real agent, a real plan reader, the Keychain or the network.
- A bug fix comes with a test that fails without it. Test the behaviour a
  person or an agent sees (the API, the CLI's output and exit code, the
  files on disk), not internal steps.
- Concurrency matters here (sessions, the bus, locks): new shared state is
  covered by a test under `-race`.

## What must not weaken

The service runs agents on the person's machine under the person's
sign-ins. Changes near these need a test that proves the limit still
holds, and a mention in the stage's decisions:

- The rules for agents' commands and approvals (`internal/serve/policy.go`,
  `shellparse.go`): anything they cannot judge goes to the person.
- Claude's sandbox for sessions (`sandbox.go`) and the files it protects.
- Billing: no session bills an API key unless its profile says
  `api_billing = true`; API keys are stripped from agents' environments.
- The person's approvals: the dispatcher never answers one; remote devices
  confirm dangerous ones with a passkey; push, merge to a remote, publish
  and deploy are never done by the service.
- The service removes no worktree or branch on its own; cleanup moves a
  worktree to the trash and deletes nothing in it.
- Secrets (the API token, Telegram's token, VAPID keys, device
  credentials) stay in the store with mode 0600 and out of logs, feeds and
  errors.

## Style

- `gofmt`; small functions; errors returned with context, never ignored
  where they change behaviour.
- Comments say why, in plain English, as the existing code does.
- Messages the person reads (CLI, board, feed) say what happened and what
  to do next.
- New dependencies are a decision for the maintainer: say what one adds
  and what writing it would cost.

## Git

- One topic per branch and commit; the message's first line says what the
  change does (`Conveyor stage N: …` for a stage).
- No AI or tool attribution lines in commits (no `Co-Authored-By` for
  agents).
- Agents commit locally; pushing, tagging a release and merging pull
  requests are the maintainer's.
