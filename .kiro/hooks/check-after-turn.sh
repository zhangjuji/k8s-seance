#!/usr/bin/env bash
# stop hook: runs when Kiro finishes a turn. If any Go file or go.mod/go.sum
# differs from HEAD (or is untracked), or was changed by a commit made during
# the turn (HEAD recorded by mark-turn-start.sh), runs gofmt -l, go vet and
# go test on the whole module. Otherwise it only logs a skip, so chat-only
# turns stay cheap. Exit 1 makes Kiro show the failure on stderr.
set -u
. "$(dirname "$0")/lib.sh"
cat >/dev/null

cd "$(repo_root)" || exit 1
changed="$(git status --porcelain -- '*.go' go.mod go.sum 2>/dev/null | wc -l | tr -d ' ')"
state="$(turn_state_file)"
if [ -f "$state" ]; then
	start="$(cat "$state")"
	rm -f "$state"
	if [ -n "$start" ] && [ "$start" != "$(git rev-parse HEAD 2>/dev/null)" ]; then
		committed="$(git diff --name-only "$start" HEAD -- '*.go' go.mod go.sum 2>/dev/null | wc -l | tr -d ' ')"
		changed=$((changed + committed))
	fi
fi
if [ "$changed" = "0" ]; then
	hook_log stop skip "no Go changes"
	exit 0
fi

fail=""
unformatted="$(gofmt -l . 2>&1)"
[ -n "$unformatted" ] && fail="${fail} gofmt"
vet_out="$(go vet ./... 2>&1)" || fail="${fail} vet"
test_out="$(go test ./... 2>&1)" || fail="${fail} test"

if [ -z "$fail" ]; then
	hook_log stop ok "changed=${changed} gofmt+vet+test passed"
	exit 0
fi
hook_log stop fail "changed=${changed} failed:${fail}"
{
	echo "check-after-turn failed:${fail}"
	[ -n "$unformatted" ] && echo "gofmt -l:" && echo "$unformatted"
	case "$fail" in *vet*) echo "$vet_out" | tail -20 ;; esac
	case "$fail" in *test*) echo "$test_out" | grep -v '^ok ' | tail -30 ;; esac
} >&2
exit 1
