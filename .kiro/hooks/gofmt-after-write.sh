#!/usr/bin/env bash
# postToolUse hook (matcher "write"): runs after Kiro's write or code tool,
# which is the CLI's closest equivalent to an IDE "file saved" event.
# Checks the written file with gofmt -l. It never rewrites the file.
# Exit 1 makes Kiro show the stderr warning; the tool has already run.
set -u
. "$(dirname "$0")/lib.sh"

event="$(cat)"
path="$(printf '%s' "$event" | jq -r '.tool_input.path // .tool_input.file_path // empty' 2>/dev/null)"
tool="$(printf '%s' "$event" | jq -r '.tool_name // "?"' 2>/dev/null)"

case "$path" in
*.go) ;;
*)
	hook_log postToolUse:gofmt skip "tool=${tool} path=${path:-none}"
	exit 0
	;;
esac

if [ ! -f "$path" ]; then
	hook_log postToolUse:gofmt skip "tool=${tool} path=${path} (missing)"
	exit 0
fi

if out="$(gofmt -l "$path" 2>&1)" && [ -z "$out" ]; then
	hook_log postToolUse:gofmt ok "tool=${tool} path=${path}"
	exit 0
fi
hook_log postToolUse:gofmt fail "tool=${tool} path=${path}"
echo "gofmt: ${path} is not formatted (run: gofmt -w ${path}) ${out}" >&2
exit 1
