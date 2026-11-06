# Acceptance of the rewrite (2026-11-06)

## Scope

This pass checks the `kiro-rewrite` branch against spec.md §7 and the features in the README.

- Start HEAD: `f9d5d83`. Safety fix found and committed during this pass: `c306061 fix(rite): refuse casts whose Host is not loopback (DNS rebinding)`.
- What was run: the Go gates, two HTTP smoke scripts, a headless browser run with screenshots, a secret scan, and nested Kiro CLI runs to re-check the hooks.
- Everything ran on 127.0.0.1. The data came from `--fixtures --replay`, a local fake Kubernetes API and a local fake OpenAI-compatible endpoint.
- No real cluster was used and no real LLM was called.

The evidence is outside the repository, in `/workspace/m3-rewrite/acceptance/`. File names below are relative to that directory.

## Gates

| Check | At `f9d5d83` | At `c306061` |
| --- | --- | --- |
| `go build ./...` | pass | pass |
| `go vet ./...` | pass | pass |
| `gofmt -l .` | clean | clean |
| `go test -count=1 ./...` | pass | pass |
| `go test -race -count=1 ./...` | pass | pass |
| `bash -n` on the scripts | pass | pass |
| Evidence | `gates.txt` | `gates-c306061.txt` |

The toolchain was `go1.27.3 linux/amd64`.

| Smoke run | Result | Evidence |
| --- | --- | --- |
| Fixtures and replay over HTTP | 47 pass, 0 fail | `smoke/smoke-fixtures.txt` |
| Fake Kubernetes API over HTTP | 26 pass, 0 fail | `smoke/smoke-cluster-fake.txt` |
| Headless browser, scenarios S1–S6 | 26 pass, 0 fail | `smoke/browser.txt`, `smoke/browser/*.png` |

## spec.md §7

Statuses are copied from `CHECKLIST.md`.

| # | Item | Status | Evidence |
| --- | --- | --- | --- |
| 1 | Fresh kind cluster and `kill-them-all.sh` within 2 minutes | BLOCKED | `kind-blocker.txt`: `instabox: no sandbox with ID or name "box-9cc212"`. No retry elsewhere. |
| 2 | List shows 3 spirits with badges 3/3 | PASS on fixtures | `smoke/smoke-fixtures.txt` §7.2, browser S1. The kind form of this item is BLOCKED. |
| 3 | Each opening names the correct cause and cites existing `[E#]` | PASS on replay | `smoke/smoke-fixtures.txt` §7.3 for all 3 spirits, browser S2 and S4 |
| 4 | "谁删了你？" answer says there is no record | PASS on replay | `smoke/smoke-fixtures.txt` §7.4 |
| 5 | Postmortem has 7 sections, a copy button and a `.md` attachment | PASS | `smoke/smoke-fixtures.txt` §7.5 for all 3 spirits, browser S2 |
| 6 | No write calls outside `internal/rite`, read-only RBAC | PASS static / BLOCKED live | `internal/readonly_test.go` and `rbac_test.go` pass in the gates. Against the fake API, default mode sent no write. `kubectl auth can-i` on a cluster is BLOCKED. |
| 7 | `--fixtures --replay` runs the full demo with no cluster and no key | PASS | `smoke/smoke-fixtures.txt` (run with `env -i`, no `SEANCE_LLM_*`), browser fixtures server |
| 8 | At most 6 calls and 30000 input tokens per séance | PASS | 19748 to 21477 of 30000 used. A 7th request gets the exhausted line and a `budget_exhausted` template. Mock is used without configuration. The configurable endpoint was exercised with the fake LLM in browser S5. |
| 9 | Public repository, LICENSE, `.kiro/` | N/A here | `.kiro/` and LICENSE are in the tree. Publication was not checked or done. |
| 10 | Video, Devpost, blog, social posts | NOT DONE | Not marked complete. |

## Features

"fake API" means the local fake Kubernetes API. "fake LLM" means the local fake OpenAI-compatible endpoint. "unit tests only" means it was not run end to end in this pass.

| Feature | Status | How tested |
| --- | --- | --- |
| Spirit list and cause badges | PASS | Fixtures over HTTP, browser S1 |
| Séance chat, SSE streaming, `[E#]` citations | PASS | Replay over HTTP, browser S2 |
| Postmortem | PASS | Replay over HTTP, browser S2 |
| Budget, exhausted line, template fallback | PASS | Replay over HTTP |
| Summoning bell: arrivals, mass grave, pre-existing spirit not announced, reconnect, sound off by default | PASS (fake API) | `smoke/smoke-cluster-fake.txt`, browser S6 |
| Bell "died again" notice and double ring for returns | unit tests only | `internal/web/live_test.go`, `returns_test.go` |
| Forgetting countdown: API fields, list column, séance panel, `memory.js` ticking | PASS | Fixtures, fake API, browser S1 and S2 |
| Countdown fading below 15 minutes and flicker below 5 minutes | unit tests only | Thresholds were not reached in this run |
| Spirit board: planchette, YES, NO on a lie, GOODBYE, 390 px mobile fit | PASS | Browser S2 and S5 |
| Lie detector: unknown `[E99]`, exit code 42, restarts 99, 512Mi flagged | PASS (fake LLM) | Browser S5. Recorded replies gave 0 false positives. |
| Lie detector: timestamps, advice-sentence exemption | unit tests only | `internal/spirit` fact check tests |
| Resurrection spell: OOM patch to 128Mi, `--dry-run=server` first | PASS | Fixtures over HTTP |
| Resurrection spell: release broke it, `rollout undo` first | PASS (fake API) | `smoke/smoke-cluster-fake.txt` |
| Casting off by default: cast and vitals 403 | PASS | Fixtures and a cluster spirit on the fake API. No write reached the fake API. |
| Casting on: rebinding Host 403, cross-site 403, cast before rehearsal 409, rehearsal sends `dryRun=All` only, no confirm 400 | PASS (fake API) | `smoke/smoke-cluster-fake.txt` run 2 |
| Real cast, vitals revived or fell again, revived mark | unit tests only | Fake-clientset tests in `internal/rite` and `internal/web`. Real cluster BLOCKED. |
| Family and rebirths: regression verdict, `MODE` normal to turbo diff, panel | PASS (fake API) | `smoke/smoke-cluster-fake.txt`, browser S6 |
| Family: instance and whole-family verdicts, StatefulSet and DaemonSet | unit tests only | `internal/k8s` lineage tests |
| Graveyard, ofrenda, photo page, enshrined mark, file mode 0600 | PASS | Fixtures over HTTP, browser S3 |
| Offerings | PASS | 3 per fixture spirit. Lineage offering on the fake API. |
| Returns: 1 return recorded, row shows 第 1 次归来 | PASS (fake API) | `smoke/smoke-cluster-fake.txt` |
| English UI, `seance_lang` cookie (HttpOnly), English openings, English altar | PASS | Fixtures over HTTP, browser S4 |
| No page exceptions or console errors | PASS | All browser scenarios |

## Safety

`safety-review.md` read the casting code and its tests. It found one gap.

- **Finding.** The cross-site check compared `Origin` with the request's own `Host` header. A page on another domain that rebinds its DNS name to 127.0.0.1 sends the same name in both headers and `Sec-Fetch-Site: same-origin`, so the check passed. With `--allow-resurrect` on, such a page could rehearse and then cast. A probe in a throwaway clone returned status 200, and 1 write reached the fake clientset (`safety-rebind-probe-before.txt`).
- **Fix.** `c306061` makes cast and vitals also require a `Host` header that names a loopback address (`localhost`, `127.0.0.1`, `[::1]`, any port). It adds `TestCastRefusesNonLoopbackHost` and `TestLoopbackHost`, and updates README.md and requirement R19.1. A nested Kiro run wrote it (`fix-rebind/`).
- **After.** A rebinding Host gets 403 with 0 writes. A loopback Host gets 200 with 1 dry-run write (`safety-rebind-probe-after.txt`). The fake API smoke run confirms 403 for a rebinding Host on cast and vitals.
- **Not changed.** Read-only pages (evidence, postmortems) can still be read through DNS rebinding. That was already true before casting existed.
- **Hook gap seen.** The fix was committed as `git -c user.name=... commit`, which does not contain the text `git commit`, so `guard-commit.sh` did not check it. The stop hook ran gofmt, vet and tests on the change and passed. This is the limitation already listed in `docs/how-we-used-kiro.md`.
- **Secret scan.** Generic patterns over the tracked files, the rewrite history, the commit messages and the evidence files found 4 hits (`secret-scan.txt`). All 4 are one synthetic test string used as redaction test input. No exact-key scan was run, because it would need the key.

## Kiro use in this pass

| Work | Model | Basis |
| --- | --- | --- |
| Nested `kiro-cli` runs in this acceptance (hook re-checks, safety fix) | `claude-opus-5.5`, `--effort medium` | `hooks-opus/*.cmd`, `hooks-opus/session-model-attestation.txt`, `fix-rebind/cmd.txt` |
| The 14 nested runs of task 20 (11 hooks, 3 MCP) | `claude-haiku-5.5` | `model-usage.md`. This is an exception to the opus-only rule. |
| Top-level rewrite sessions (tasks 8, 10–19, 9.3, task 20) | `claude-opus-5.5`, effort medium | Stated by the coordinator. The session logs do not record the model. |

The hook re-checks ran `kiro-cli 2.37.0 chat --agent seance-dev --model claude-opus-5.5 --effort medium` in a throwaway clone of `f9d5d83` with its remote removed (`hooks-opus/SUMMARY.md`).

| Run | Result |
| --- | --- |
| Unformatted `.go` file, then commit | Commit blocked by gofmt, stop hook failed. HEAD unchanged. |
| Failing test, then commit | Commit blocked by `go test`. HEAD unchanged. |
| Passing test, then commit | Commit allowed (in the clone only), stop hook passed. |
| Interactive session in a pseudo-terminal | `agentSpawn` fired. |

Not re-checked: the parallel write-and-commit race and the missing `agentSpawn` in `--no-interactive` runs.

`.kiro/agents/seance-dev.json` and `.kiro/steering/tech.md` still name `claude-haiku-5.5` as the default. The runs above override it on the command line.

Update after this acceptance: both files now name `claude-opus-5.5`, and `.kiro/settings/cli.json` sets its default effort to `medium`.

## Parity with main

`parity/PARITY.md` compares the rewrite with the README, requirements and tasks of `main` at `638aa4a`. No source code of `main` was read. Both branches list the same 19 requirements. The rewrite adds R6.2 (30000 total input) and R19.1 (loopback Host).

- **Tested** in this pass: list, chat, "no record" answer, postmortem, budget, spells, casting off by default, forgetting countdown, spirit board, ofrenda, offerings and English. The bell, casting on, family, returns and the lie detector were tested against the fake API or fake LLM only.
- **Documented** and unit-tested, not run end to end: the "died again" bell notice, a real cast and vitals, countdown fading, the other family verdicts, and timestamp checks in the lie detector.
- **Blocked:** the kind demo and `kubectl auth can-i`.

Gaps, where `main` documents something the rewrite lacks:

| # | Gap |
| --- | --- |
| 1 | English recordings. On `main`, `<pod>-en.json` covers every call. On the rewrite it holds only the opening. English follow-ups get the English mock, and the postmortem reuses the Chinese-séance file. The rewrite README says so. |
| 2 | The rewrite README has no lie-detector section. The feature exists and was tested. |
| 3 | The rewrite README has no notes for OrbStack, k3s or Docker Desktop. It covers kind only. |
| 4 | `docs/submission/` (blog, Devpost, social, storyboard) is not on the rewrite. |

## Known exceptions

- **Kind blocked.** The sandbox for the kind cluster was not available (`kind-blocker.txt`). §7.1, the kind form of §7.2, the live part of §7.6 and casting on kind were not run.
- **Real cast not exercised.** No confirmed cast was sent, and the fake API would have refused it. Revived and fell-again detection and the revived mark are covered by fake-clientset unit tests only.
- **Task 10 key temp file.** During task 10, a key scan wrote the real API key to a `mktemp` file in `/tmp`, on a persistent ext4 volume, with mode 0600. The script then ran `rm -f` on it. The output printed after that line shows the shell reached it, but the exit status of `rm -f` was not recorded, so the removal is not provable. It is not claimed that the key never touched disk. `rm` does not overwrite the freed blocks; if that matters, rotate the key. No other Kiro session transcript writes a key to a `mktemp` file. The committed `demo/record-english.sh` passes the key through a pipe. Details: `task10-keyfile-investigation.md`.
