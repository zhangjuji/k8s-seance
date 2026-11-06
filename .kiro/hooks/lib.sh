# Shared helpers for the Kiro CLI hooks in this directory. Sourced, not run.
#
# Every hook appends one line to the sentinel log so a run can prove which
# hooks fired:  <UTC time> <session> <hook> <result> <detail>
# The log defaults to a file in $TMPDIR, outside the repository. Set
# SEANCE_HOOK_LOG to move it.

SEANCE_HOOK_LOG="${SEANCE_HOOK_LOG:-${TMPDIR:-/tmp}/k8s-seance-kiro-hooks.log}"

hook_log() {
	# hook_log <hook> <result> <detail...>
	local hook="$1" result="$2"
	shift 2
	printf '%s session=%s SEANCE_HOOK %s %s %s\n' \
		"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${KIRO_SESSION_ID:-none}" \
		"$hook" "$result" "$*" >>"$SEANCE_HOOK_LOG"
}

# turn_state_file prints the file where mark-turn-start.sh keeps the HEAD
# commit of the current turn, one file per Kiro session.
turn_state_file() {
	printf '%s/k8s-seance-kiro-turn-%s' "${TMPDIR:-/tmp}" "${KIRO_SESSION_ID:-none}"
}

# repo_root prints the repository root, or the current directory outside git.
repo_root() {
	git rev-parse --show-toplevel 2>/dev/null || pwd
}
