# Acceptance, second pass (task 9.3)

Scope of this pass: spec.md §7.8, the per-séance LLM budget. One séance (opening, follow-ups and one postmortem) makes at most 6 calls, each call sends at most 6000 estimated input tokens, and the séance sends at most 30000 estimated input tokens in total (`SEANCE_BUDGET_MAX_TOTAL_INPUT_TOKENS` overrides the total). The other items of §7 are not re-checked here.

## Rules (`internal/llm/budget.go`)

- Input is estimated as half the rune count of the messages and evidence. Each call adds the larger of the estimate and the input tokens the provider reported.
- The input limit of a call is the smaller of 6000 and what is left of the total. Older history is dropped first.
- A request that does not fit 6000 even without history is refused with `ErrInputTooLarge` and is not counted.
- A request that fits 6000 but not the rest of the total ends the séance's budget: the ghost says the exhausted line, `budgetRemaining` becomes 0, and the postmortem uses the rule template with `budget_exhausted`.
- `GET /api/seances/{id}` and the JSON reply of `POST /api/seances/{id}/messages` include `inputTokensUsed` and `inputTokensMax`.

## Checks

| Check | How | Result |
| --- | --- | --- |
| Defaults are 6 calls, 6000 per call, 30000 total | `internal/llm/budget_total_test.go` | pass |
| Total cap refuses as exhausted, history trimmed first, reported input counted when larger, env override | `internal/llm/budget_total_test.go` | pass |
| Opening, 4 follow-ups and a postmortem stay within 6 calls and 30000 | `internal/web/budget_total_test.go`, and `--fixtures --replay` over HTTP: 6 calls, 19291 of 30000 for `memory-glutton-7cc6df9966-bf87c` | pass |
| Total cap reached after the opening: exhausted line, postmortem `template` / `budget_exhausted` | `internal/web/budget_total_test.go`, and HTTP with `SEANCE_BUDGET_MAX_TOTAL_INPUT_TOKENS` = opening + 1 | pass |
| An invalid override (`lots`) keeps 30000 | HTTP | pass |
| Casting stays off without `--allow-resurrect` | HTTP: `vitals` and `cast` answer 403 | pass |
