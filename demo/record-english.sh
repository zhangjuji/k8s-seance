#!/usr/bin/env bash
# Record the English opening of each fixture spirit into
# fixtures/replay/<pod>-en.json with a real OpenAI-compatible endpoint
# (Requirement 10.3, task 10.4).
#
# The endpoint comes from the environment, as for the server:
#
#   export SEANCE_LLM_BASE_URL=https://api.deepseek.com
#   export SEANCE_LLM_MODEL=deepseek-flash
#   export SEANCE_LLM_API_KEY=...        # never written anywhere by this script
#   ./demo/record-english.sh
#
# The script builds the binary, starts it for the length of the run on
# 127.0.0.1 (SEANCE_RECORD_ADDR, default 127.0.0.1:16670) with --fixtures and
# SEANCE_RECORD=1, summons each fixture spirit in English and waits for its
# opening, then stops the server. Only the opening is requested, so each spirit
# costs at most one call. An opening that is already recorded is replayed from
# its file with no call. The server runs with a temporary HOME, so it reads no
# config file, and with the graveyard off. Its log goes to SEANCE_RECORD_LOG
# when that is set, and is deleted otherwise. The key is never printed: the
# last step only counts how often it occurs in the files it wrote.
set -euo pipefail

cd "$(dirname "$0")/.."

for v in SEANCE_LLM_BASE_URL SEANCE_LLM_API_KEY SEANCE_LLM_MODEL; do
	if [ -z "${!v:-}" ]; then
		echo "record-english: $v is not set" >&2
		exit 1
	fi
done

ADDR=${SEANCE_RECORD_ADDR:-127.0.0.1:16670}
BASE="http://$ADDR"
WORK=$(mktemp -d)
SERVER_PID=""

cleanup() {
	if [ -n "$SERVER_PID" ]; then
		kill -INT "$SERVER_PID" 2>/dev/null || true
		wait "$SERVER_PID" 2>/dev/null || true
	fi
	if [ -n "${SEANCE_RECORD_LOG:-}" ] && [ -f "$WORK/server.log" ]; then
		cp "$WORK/server.log" "$SEANCE_RECORD_LOG"
	fi
	rm -rf "$WORK"
}
trap cleanup EXIT

go build -o "$WORK/seance" ./cmd/seance

HOME="$WORK" SEANCE_RECORD=1 SEANCE_LLM_MODE=http \
	timeout 600 "$WORK/seance" --fixtures -addr "$ADDR" -graveyard "" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
	curl -fsS "$BASE/healthz" >/dev/null 2>&1 && break
	sleep 0.1
done
curl -fsS "$BASE/healthz" >/dev/null

PODS=$(curl -fsS "$BASE/api/spirits?src=fixtures" |
	python3 -c 'import json,sys; [print(s["namespace"], s["name"]) for s in json.load(sys.stdin)["spirits"]]')

status=0
while read -r ns pod; do
	[ -n "$pod" ] || continue
	id=$(curl -fsS -H 'Content-Type: application/json' \
		-d "{\"ns\":\"$ns\",\"pod\":\"$pod\",\"src\":\"fixtures\",\"lang\":\"en\"}" \
		"$BASE/api/seances" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
	curl -sS --max-time 120 "$BASE/api/seances/$id/stream?once=1" >"$WORK/stream-$pod.txt" || true
	if grep -q '^event: turn' "$WORK/stream-$pod.txt"; then
		echo "record-english: $pod: opening received"
	else
		echo "record-english: $pod: no opening (see the server log)" >&2
		status=1
	fi
done <<<"$PODS"

kill -INT "$SERVER_PID"
wait "$SERVER_PID" || true
SERVER_PID=""

# Count, never print, the key in what this run wrote.
key_hits=0
for f in fixtures/replay/*-en.json "$WORK/server.log" "$WORK"/stream-*.txt; do
	[ -f "$f" ] || continue
	n=$(grep -cFf <(printf '%s\n' "$SEANCE_LLM_API_KEY") "$f" || true)
	key_hits=$((key_hits + n))
done
echo "record-english: API key occurrences in written files: $key_hits"
if [ "$key_hits" -ne 0 ]; then
	status=1
fi
exit "$status"
