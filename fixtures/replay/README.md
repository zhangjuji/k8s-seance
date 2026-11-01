# fixtures/replay

Recorded spirit replies, read by `llm.ReplayProvider` when the CLI runs with
`--replay` (or `SEANCE_LLM_MODE=replay`). The directory is embedded into the
binary with `go:embed`, so the CLI needs no network and no API key to use it.

## Format

One file per fixture: `<fixture-id>.json`. The fixture ID is the pod name,
lowercased, with every character outside `[a-z0-9-_]` replaced by `-`.

```json
{
  "fixtureId": "memory-glutton-7cc6df9966-bf87c",
  "turns": [
    {
      "turnIndex": 0,
      "cause": "OOMKilled",
      "lastUser": "opening",
      "reply": "我的内存一点点涨满……「lastState: terminated ... reason=OOMKilled」[E2]。",
      "usage": { "inputTokens": 812, "outputTokens": 96 }
    }
  ]
}
```

- `fixtureId` must equal the pod name the CLI passes in. A mismatch counts as a miss.
- `turnIndex`: 0 is the opening, 1.. are follow-ups, 100 is the postmortem.
- `lastUser`: the question a follow-up was asked with. Replay matches follow-ups
  by this text (see Lookup below).
- `cause` is for people reading the file. Replay does not use it.
- `reply` is streamed back in small chunks. `usage` is returned as recorded.

## Lookup

Replay finds the recorded turn for a request in this order:

1. Opening (`turnIndex` 0) and postmortem (`turnIndex` 100): the recorded turn
   with the same `turnIndex`. The question text is not compared, because the
   postmortem prompt embeds the whole transcript.
2. Follow-up (any other `turnIndex`): a recorded turn with `turnIndex` from 1 to
   99 whose `lastUser` equals the request's last user message. The request's
   `turnIndex` does not have to match the recorded one, so a question asked
   later in a session still gets the recorded answer. If several turns match,
   the lowest `turnIndex` wins.

Both texts go through the same normalization before they are compared. Leading
and trailing spaces are removed. Full-width `？！，。` count as `?!,.`. Trailing
`?`, `!`, `.`, `,` and spaces are removed. ASCII letters ignore case. So
`怎么修复？`, `怎么修复?` and ` 怎么修复 ？ ` all match the same recording.

A request with no match gets the mock reply instead. A file that is missing or
is not valid JSON also falls back to mock, with no error.

## How to record

Recording needs a real OpenAI-compatible endpoint. Set the three settings
through environment variables or `~/.k8s-seance/config.yaml`:

```sh
export SEANCE_LLM_BASE_URL=...   # provided by the endpoint owner
export SEANCE_LLM_API_KEY=...    # keep it in the environment, not in files
export SEANCE_LLM_MODEL=...
SEANCE_RECORD=1 go run ./cmd/seance -n seance-demo
```

Each HTTP reply is saved into this directory, and an existing turn with the
same `turnIndex` is replaced. Mock replies are never recorded. Commit the
resulting files so the offline demo works for everyone.
