# How we used Kiro

This file describes how Kiro CLI was used on the `kiro-rewrite` branch and what was verified. It separates what the repository proves (files and git history) from what was observed only in local runs.

## Where the code came from

- **Base (tasks 1–7, 9.1, 9.2).** The first 37 commits, up to `e5e749e chore(tasks): 9.1 acceptance run and 9.2 README done`, are dated 2026-11-01. They existed before the rewrite and were not rebuilt. Most of their subjects name the task they implement, for example `f41809b ... (task 1.1)` and `2e9cb5d feat(llm): per-seance budget, opening cache, history trim (4.3)`. The local launcher used for numbered task runs calls `kiro-cli chat --no-interactive --model claude-haiku-5.5`. This document does not claim that every line of the base was written by Kiro.
- **Rebuilt (tasks 8, 10–19, 9.3).** On 2026-11-06, `310c9b5 docs(spec): prepare Kiro implementation plan` updated the spec. Then each task was rebuilt independently from `.kiro/specs/k8s-seance/` in its own Kiro CLI session, one commit per task: `1a7ea6c` (8), `7a2ba61` (10), `be4d69e` (11), `19d655a` (12), `9105f01` (13), `22b5d29` (14), `3f03e1b` (15), `16d4495` (16), `65c638c` (17), `ca34e77` (18), `e44a0ac` (19) and `fcaf865` (9.3). The session logs of these runs do not record the model.
- **Kiro tooling (task 20).** The hooks, property tests, MCP configuration and this document were written in one Kiro CLI session running `claude-opus-5.5`. The nested `kiro-cli` runs it used to test hooks and MCP ran `claude-haiku-5.5`. No product feature was added in task 20. The only product change is the redaction fix described under [Property tests](#property-tests).

`.kiro/steering/tech.md` names Kiro CLI with `claude-haiku-5.5` as the default coding tool.

## Specs and steering

- `.kiro/specs/k8s-seance/requirements.md`: numbered requirements with acceptance criteria (EARS style).
- `.kiro/specs/k8s-seance/design.md`: architecture, interfaces, data models, error handling, testing strategy, and the exact properties of the property tests.
- `.kiro/specs/k8s-seance/tasks.md`: the implementation plan with checkboxes. Task 20 covers this tooling.
- `.kiro/steering/product.md`, `tech.md`, `structure.md`: product rules (read-only by default, low cost, works offline), the stack and the directory layout.

## Hooks

Kiro CLI 2.37.0 (`kiro-cli --version`) reads hooks only from an agent config. Its triggers are `agentSpawn`, `userPromptSubmit`, `preToolUse`, `postToolUse` and `stop`. The scripts live in `.kiro/hooks/` and are registered in the workspace agent `.kiro/agents/seance-dev.json`. `kiro-cli agent validate --path .kiro/agents/seance-dev.json` exits 0.

```sh
kiro-cli chat --agent seance-dev   # from the repository root
```

| Script | Trigger (matcher) | Check |
| --- | --- | --- |
| `spawn-context.sh` | `agentSpawn` | Adds branch, Go version, dirty files and two project rules to the context |
| `mark-turn-start.sh` | `userPromptSubmit` | Saves `HEAD` so the stop hook can see commits made during the turn |
| `gofmt-after-write.sh` | `postToolUse` (`write`) | `gofmt -l` on the Go file Kiro just wrote; warns, never rewrites |
| `guard-commit.sh` | `preToolUse` (`shell`) | For a command containing `git commit`: `gofmt -l .`, `go vet ./...`, `go test ./...`; exit 2 blocks the command |
| `check-after-turn.sh` | `stop` | If Go files changed (working tree or commits made in the turn): `gofmt -l .`, `go vet ./...`, `go test ./...` |

### Save and pre-commit events

Kiro CLI has no file-save trigger and no git pre-commit trigger. File-event hooks belong to the Kiro IDE and use a different format. None are included here, and none are claimed as verified.

- In place of "on save", `gofmt-after-write.sh` runs after each Kiro `write` or `code` tool call. Files edited in an editor are not checked.
- In place of "pre-commit", `guard-commit.sh` checks the shell commands Kiro runs. A `git commit` typed in a terminal is not checked. A real git hook was not added, because that would apply to every committer and not only to Kiro.

### How triggering was verified

Each hook appends a line with `SEANCE_HOOK` to `$SEANCE_HOOK_LOG`. Every nested run was bounded with `timeout`, worked in a throwaway copy of the repository in `/tmp`, used `claude-haiku-5.5`, and made no LLM call from K8s Séance itself. Lines from those logs:

```
SEANCE_HOOK postToolUse:gofmt fail tool=write path=internal/llm/zz_hookprobe.go
SEANCE_HOOK preToolUse:commit block gofmt: internal/llm/zz_hookprobe.go
SEANCE_HOOK stop fail changed=1 failed: gofmt
SEANCE_HOOK userPromptSubmit ok head=fcaf865
SEANCE_HOOK preToolUse:commit block go test failed
SEANCE_HOOK preToolUse:commit allow gofmt+vet+test clean
SEANCE_HOOK stop ok changed=1 gofmt+vet+test passed
SEANCE_HOOK agentSpawn ok branch=kiro-rewrite
```

| Run | What it showed |
| --- | --- |
| Unformatted write, then `git commit` | `postToolUse` warned, `preToolUse` blocked the commit (the model reported the block), `stop` failed on gofmt. No commit was created. |
| Formatted file, then commit | Commit allowed. The stop hook skipped because the change was already committed. This gap led to `mark-turn-start.sh`. |
| Write and commit in one parallel batch | `guard-commit.sh` ran before the file existed and allowed the commit. This race is why the stop hook also checks commits made during the turn. |
| Write a deliberately failing test | `stop` failed on `go test`. |
| Commit that failing test | `preToolUse` blocked it with `go test failed`. |
| Passing test, written and committed in one turn | Guard allowed it, and `stop` ran the checks on the committed file (`changed=1`, passed). |
| `--no-interactive`, object and array formats | `agentSpawn` did not fire, in either format. `userPromptSubmit` fired and its stdout reached the model. |
| Interactive session in a pseudo-terminal | `agentSpawn` fired (`agentSpawn ok`). |

## Property tests

The tests use the standard library `testing/quick` with fixed seeds and fixed case counts, so every run checks the same inputs. A failure logs the seed. The properties are written out in `design.md` under 性质测试 (property tests), and each test names its property ID.

| File | Properties |
| --- | --- |
| `internal/autopsy/property_test.go` | P-C1..P-C6: the classifier is total and deterministic, follows the documented rule order, sets `Secondary` only as specified, ignores container and event order, picks `LastExitCode` as specified, and never treats exit 137 alone as OOM. P-E1..P-E8: evidence IDs have no gaps, the size cap holds, truncation only cuts the tail, duplicate collapsing and log line selection match the spec exactly, and redaction removes every secret shape while keeping key names and is idempotent. |
| `internal/llm/property_test.go` | P-B1..P-B5 over 3000 random scenarios: call cap, per-call and per-séance input caps, `InputUsed` accounting, output token clamping, trimming (evidence, system messages and the last user message are kept, oldest dropped first), and exhaustion being sticky. The test fails if any outcome (exhausted, too large, forwarded, trimmed, clamped, postmortem) never occurs. P-B6: the call cap holds with 2–25 concurrent callers. P-H1..P-H3: `TrimHistory`. |
| `internal/postmortem/property_test.go` | P-H4: transcript `truncate`. |

**Bug found.** P-E6 failed on `token: Bearer <value>`. The key=value rule ran first and replaced only the word `Bearer`, so the token itself stayed in the evidence sent to the LLM. The fix (`c81fff8`) runs the Bearer rule first, and `TestBuildEvidenceRedaction` has a regression row for it.

**Mutation check.** Nine small faults were injected one at a time into a copy of the repository: call cap off by one, history round boundary, the oldest-first drop, output clamping, the evidence size check, duplicate collapsing, the exit-137 rule, the `ErrImagePull` reason and the `truncate` boundary. The property tests caught all nine. The call cap mutant was caught only after P-B6 (concurrent callers) was added, because sequential calls never reach that check.

## MCP

`.kiro/settings/mcp.json` starts `gopls mcp`, the MCP server built into the Go language server. It runs locally over stdio and needs no token or network account. `go_rename_symbol` and `go_vulncheck` are disabled: rename proposes source edits, and vulncheck downloads the vulnerability database. `GOPROXY=off` and `GOTOOLCHAIN=local` keep it from downloading modules or toolchains. The remaining tools (`go_diagnostics`, `go_file_context`, `go_package_api`, `go_search`, `go_symbol_references`, `go_workspace`) read the workspace.

A custom agent does not load the workspace `mcp.json`. A `seance-dev` run listed no `go_` tools until the same server was added to `mcpServers` in `seance-dev.json`.

Verified with bounded nested runs (`--trust-tools=@gopls`, all other tools untrusted):

- Default agent: it listed the 6 enabled tools and called `go_package_api` once for `internal/llm`. It quoted `func NewBudget(p Provider, l Limits) *Budget` from the result. The session record shows one MCP call, `toolName: go_package_api, serverName: gopls`.
- `seance-dev`: it listed the same 6 tools and called `go_symbol_references` once for `autopsy.Redact`. The reported references in non-test files match `grep -rn 'Redact('`. The session record shows one MCP call to `gopls`.
- `git status` was unchanged after each run.

Setup: `go install golang.org/x/tools/gopls@v0.24.0`, with `$(go env GOPATH)/bin` on `PATH`.

## Limitations

- Hooks run only in `kiro-cli chat --agent seance-dev` started from the repository root. They need `bash`, `git`, `jq` and `go`.
- `agentSpawn` did not fire in `--no-interactive` runs of Kiro CLI 2.37.0.
- Edits made outside Kiro and commits typed in a terminal are not checked.
- `guard-commit.sh` matches the text `git commit`. A commit run another way, such as through a script, is caught only by the stop hook at the end of the turn.
- The stop and commit hooks run the whole test suite, which takes a few seconds here and has a 300-second timeout.
- MCP needs `gopls` on `PATH`. Without it, the server fails to start and Kiro runs without the `go_` tools.
- The evidence of these runs (hook logs, nested run output, mutation results) was saved outside the repository. The repository holds the scripts, configs, tests and this description.
- Nested Kiro invocations used for this verification: 14 (11 for hooks, 3 for MCP).
