#!/usr/bin/env bash
# preToolUse hook (matcher "shell"): Kiro CLI has no pre-commit trigger, so
# this hook inspects every shell command the agent is about to run. When the
# command contains "git commit", it runs gofmt -l, go vet and go test first
# and blocks the command (exit 2, stderr goes back to the model) if any fails.
# Commits typed outside Kiro are not covered. If the model issues a write and
# the commit in the same parallel batch, this hook can run before the write
# lands; check-after-turn.sh then catches the committed files at the stop.
set -u
. "$(dirname "$0")/lib.sh"

event="$(cat)"
cmd="$(printf '%s' "$event" | jq -r '.tool_input.command // empty' 2>/dev/null)"

case "$cmd" in
*"git commit"*) ;;
*) exit 0 ;; # not logged: every shell call passes through here
esac

cd "$(repo_root)" || exit 1
unformatted="$(gofmt -l . 2>&1)"
if [ -n "$unformatted" ]; then
	hook_log preToolUse:commit block "gofmt: $(echo "$unformatted" | tr '\n' ' ')"
	echo "Commit blocked by .kiro/hooks/guard-commit.sh: gofmt -l lists files:" >&2
	echo "$unformatted" >&2
	exit 2
fi
if ! vet_out="$(go vet ./... 2>&1)"; then
	hook_log preToolUse:commit block "go vet failed"
	echo "Commit blocked by .kiro/hooks/guard-commit.sh: go vet failed:" >&2
	echo "$vet_out" | tail -20 >&2
	exit 2
fi
if ! test_out="$(go test ./... 2>&1)"; then
	hook_log preToolUse:commit block "go test failed"
	echo "Commit blocked by .kiro/hooks/guard-commit.sh: go test failed:" >&2
	echo "$test_out" | grep -v '^ok ' | tail -30 >&2
	exit 2
fi
hook_log preToolUse:commit allow "gofmt+vet+test clean"
exit 0
