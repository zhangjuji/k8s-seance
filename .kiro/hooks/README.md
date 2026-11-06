# Kiro CLI hooks

These scripts are hooks for Kiro CLI 2.37.0. They are registered in the
workspace agent `.kiro/agents/seance-dev.json`, because Kiro CLI reads hooks
only from an agent config (`hooks` field). It does not load hook files from
this directory by itself. Start the agent from the repository root:

```sh
kiro-cli chat --agent seance-dev
```

The commands are relative paths, so a session started in another directory
will not find them. The scripts need `bash`, `git`, `jq` and `go`.

| Script | Trigger (matcher) | What it does |
| --- | --- | --- |
| `spawn-context.sh` | `agentSpawn` | Prints the branch, the Go version, uncommitted files and two project rules. Kiro adds stdout to the context. |
| `mark-turn-start.sh` | `userPromptSubmit` | Saves `HEAD` for the session in `$TMPDIR`, so the stop hook can see commits made during the turn. Prints nothing. |
| `gofmt-after-write.sh` | `postToolUse` (`write`) | Runs `gofmt -l` on the `.go` file the `write` or `code` tool just wrote. Exit 1 shows a warning. It never rewrites the file. |
| `guard-commit.sh` | `preToolUse` (`shell`) | When the shell command contains `git commit`, runs `gofmt -l .`, `go vet ./...` and `go test ./...`. Exit 2 blocks the command and sends the reason to the model. |
| `check-after-turn.sh` | `stop` | When Go files changed in the working tree or in commits made during the turn, runs `gofmt -l .`, `go vet ./...` and `go test ./...`. Exit 1 shows the failure. |

Every hook that does work appends a line containing `SEANCE_HOOK` to
`$SEANCE_HOOK_LOG` (default `${TMPDIR:-/tmp}/k8s-seance-kiro-hooks.log`).

## Events Kiro CLI does not have

Kiro CLI 2.37.0 has five triggers: `agentSpawn`, `userPromptSubmit`,
`preToolUse`, `postToolUse` and `stop`. It has no file-save trigger and no
git pre-commit trigger. The Kiro IDE's file-event hooks (`.kiro.hook` files)
are a different format, and none are included here.

- Instead of "on save", `gofmt-after-write.sh` runs after each Kiro `write`
  or `code` tool call. Edits made in an editor are not checked.
- Instead of "pre-commit", `guard-commit.sh` checks shell commands that Kiro
  runs. A `git commit` typed in a terminal is not checked.

## Observed behaviour

Checked with bounded `kiro-cli chat` runs in a throwaway copy of the
repository (evidence in `docs/how-we-used-kiro.md`):

- `userPromptSubmit`, `preToolUse`, `postToolUse` and `stop` fire with
  `--no-interactive`.
- `agentSpawn` fired only in an interactive session. It did not fire with
  `--no-interactive`, in the object format or in the array format
  (`SessionStart`).
- When the model sent the `write` and the `git commit` calls in one parallel
  batch, `guard-commit.sh` ran before the file existed and let the commit
  through. `mark-turn-start.sh` and `check-after-turn.sh` were added so the
  stop hook still checks those committed files.
