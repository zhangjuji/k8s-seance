#!/usr/bin/env bash
# userPromptSubmit hook: records HEAD when a turn starts, so the stop hook
# can also check Go files that the agent committed during the turn.
# Prints nothing, because stdout of this trigger is added to the context.
set -u
. "$(dirname "$0")/lib.sh"
cat >/dev/null

cd "$(repo_root)" || exit 0
head="$(git rev-parse HEAD 2>/dev/null)" || exit 0
printf '%s\n' "$head" >"$(turn_state_file)"
hook_log userPromptSubmit ok "head=${head:0:7}"
exit 0
