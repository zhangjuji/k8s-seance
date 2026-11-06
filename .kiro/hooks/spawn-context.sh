#!/usr/bin/env bash
# agentSpawn hook: stdout is added to the agent's context.
# Gives the agent the branch, the Go version and the dirty files, and repeats
# the two project rules that matter most when editing.
set -u
. "$(dirname "$0")/lib.sh"
cat >/dev/null # the event JSON is not needed

cd "$(repo_root)" || exit 1
branch="$(git branch --show-current 2>/dev/null || echo '?')"
echo "k8s-seance: branch ${branch}, $(go version 2>/dev/null || echo 'go not found')"
echo "Uncommitted files:"
git status --short 2>/dev/null | head -20
echo "Rules: no cluster writes outside internal/rite (internal/readonly_test.go);"
echo "update .kiro/specs/k8s-seance/ before adding a feature."
hook_log agentSpawn ok "branch=${branch}"
exit 0
