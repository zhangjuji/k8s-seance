# K8s Séance

Summon the ghost of a dead Kubernetes Pod. It tells, in the first person, how it died, using only read-only data from the cluster: pod status, exit codes, events and log tails. When the séance ends, it writes a postmortem in Markdown.

K8s Séance is a single Go binary with a local web UI. By default it never writes to the cluster. The one exception is opt-in: started with `--allow-resurrect`, it can cast a resurrection spell on a demo cluster (see [Casting](#casting-allow-resurrect)). It also runs without a cluster or an API key, using recorded fixtures.

Status: in progress (Kiroween 2026, Frankenstein track).

## What it does

- **Spirit list.** Shows every Pod that restarted or ended abnormally, with a cause badge: `OOMKilled`, `CrashLoopBackOff`, `ImagePullBackOff`, `Error`, `ProbeFailure` or `Unknown`. The cause comes from deterministic rules, not from the LLM.
- **Summoning bell.** When the list shows a cluster namespace, it stays up to date through a read-only Pod watch (`GET /api/spirits/stream?ns=`, Server-Sent Events). Watch signals are debounced for 700 ms, then the list is read again and compared with the previous one. A Pod that newly became a spirit gets a notice with its name, cause and restart count, a highlighted row and a **Summon now** button. A spirit that died again gets a notice with its restart count before and after. When 2 or more Pods die of the same cause within 60 seconds, a mass grave card lists them with the first thing to check for that cause. Spirits that were already dead when the page opened are not announced. If the watch or a list fails, the page says the connection is lost and reconnects (the watch backs off from 1 s to 30 s). The bell sound is off until you click the sound button, and it is synthesized in the browser with Web Audio. Animations stop under `prefers-reduced-motion`. Sample mode has no live stream.
- **Forgetting countdown.** Kubernetes deletes an Event one TTL after its last update (1 hour by default in kube-apiserver; set `-event-ttl` to match your cluster). The **记忆** (memory) column counts down to the first Warning event of each spirit that will be deleted. A Warning that stopped repeating, such as a failed image pull, is deleted before the BackOff events that keep being refreshed, so it is the one shown. A spirit with no Warning event shows "no event record". Below 15 minutes the row fades and a banner says to summon the spirit and write the postmortem first. Below 5 minutes the countdown flickers, and at zero it reads "forgotten". `static/memory.js` counts down in the browser every 10 seconds without polling the server, and starts again from the new values after a live list refresh. The séance page has a panel with the first key memory to fade (linked to its `[E#]`), the time when all events are gone, a warning that a container in CrashLoopBackOff keeps only one `--previous` log, a reminder that deleting the Pod loses everything, and a **留住记忆：生成复盘** (keep the memory: write the postmortem) button. Each event evidence card has its own countdown. In sample mode the countdowns start at the moment each fixture was recorded (`collectedAt`), and the page says so. `GET /api/spirits` and `GET /api/seances/{id}` include the countdown as `memory`.
- **Séance chat.** The ghost opens with a statement, then answers your questions as streamed text. Each answer cites evidence as `[E#]`. Click a citation to open the evidence card.
- **Spirit board.** The séance page has a foldable **降灵板** (spirit board). Its runes are the séance's evidence IDs, placed on one arc (up to 10) or two, with YES, NO, GOODBYE and a number row. While an answer streams, the planchette moves to each `[E#]` it cites, one step every 750 ms, and lights that evidence card. When the answer ends it moves to YES, or to NO and shakes when the fact check found a contradiction. After a postmortem it moves to GOODBYE. Answers render `**bold**` and `` `code` `` after HTML escaping, so a reply cannot inject tags. The forgetting panel starts folded and opens by itself when its key memory has less than 15 minutes left.
- **Resurrection spell.** The **念出复活咒语** (speak the resurrection spell) button on the séance page shows a fix plan built by rules from the collected autopsy, with no LLM call and no cluster call: a diagnosis that cites `[E#]`, the target object (a Deployment inferred from a ReplicaSet is marked as inferred), steps and caveats. Each step is labelled read-only, patch file, cast (writes to the cluster) or confirm, and every command that writes is preceded by the same command with `--dry-run=server`. K8s Séance never runs these commands. Values the evidence cannot give, such as a new image tag or a local config file, are shell variables like `${NEW_TAG:?}`, so the command stops when they are unset. The patch downloads as `resurrect-<pod>.yaml` (`GET /api/seances/{id}/spell.yaml`), and the JSON is at `GET /api/seances/{id}/spell`. On the kind demo, the spells for `oom.yaml` (memory limit 64Mi to 128Mi) and `crashloop.yaml` (a ConfigMap mounted at `/etc/app/config.yaml`) bring the Pods back.
- **Casting.** Off by default. With `--allow-resurrect` and a cluster spirit, the spell panel gets a cast form for the spells that a machine can run: raise the memory limit, mount the missing config file (you paste its content, it goes into a ConfigMap), set an image tag you type, roll a Deployment back to the revision that is still alive, or give a probe more time. Every cast is rehearsed first with `dryRun=All`. After a real cast, the page checks the family read-only every 3 seconds for up to 4 minutes and reports revived, fell again or waiting. A revived spirit that is on the altar gets a **🌅 已复活** (revived) mark on its photo. See [Casting](#casting-allow-resurrect).
- **Family and rebirths.** When the spirit belongs to a Deployment, StatefulSet or DaemonSet, summoning it makes one more read-only Pod list in its namespace and groups the family by `pod-template-hash` or `controller-revision-hash` into siblings (same revision) and other revisions (older: former life, newer: later life). For each other revision it compares the Pod template with the spirit's: image, command, args, environment variables, resources, probes, ports and mounts of each container, plus the ServiceAccount, nodeSelector and volumes. Environment values follow the evidence redaction rules: a variable whose name looks like a secret never shows its value, a value from a Secret or ConfigMap shows only the reference, and a hidden value that changed is shown as "changed (value hidden)". The verdict is, in this order: a living sibling means an instance problem (both nodes are shown); a living Pod of another revision while none of this revision lives means the release broke it; otherwise the whole family is dead, so check what they share. The family and the verdict become `lineage` evidence (`[E#]`, after the events), the séance page shows a **家族与转世** (family and rebirths) panel with a difference table, and `GET /api/seances/{id}` includes it as `lineage`. When the release broke it and the living revision is older, the mock opening says so and the resurrection spell rolls back first (`kubectl rollout undo` with `--dry-run=server` before it) and says that a rollback only stops the bleeding. If the list fails or is forbidden, the spirit is summoned without a family. Sample mode has no family record.
- **Postmortem.** Markdown with a copy button and a `.md` download. The timeline, evidence and impact sections come from the collected data. The LLM writes only the summary, root cause, remediation and action items. If the LLM is unavailable or the call budget is spent, a rule-based template fills those sections instead.
- **Graveyard.** Each generated postmortem also becomes a tombstone in a local JSON file: Pod name, namespace, birth and death times, cause, an epitaph (the postmortem's last words) and the postmortem itself. Generating the postmortem again for the same Pod replaces its tombstone. The **祭坛 / Ofrenda** page at `/graveyard` shows the tombstones as a Día de los Muertos altar drawn only in CSS: papel picado flags, stepped tiers with the most recent deaths on top (3, then 4, then 5 per tier), framed photos, candles and a marigold petal path. It uses folk elements only, no film characters, songs or names. Click a photo to open its page, in the same style, with the postmortem and a `.md` download. A spirit whose postmortem is on the altar shows **🌼 已供奉** (enshrined) in the spirit list, linked to its photo. Its row no longer fades and does not raise the forgetting banner, because the postmortem keeps its evidence. The match is by namespace and Pod name. Each tombstone also keeps its family (Deployment, StatefulSet or DaemonSet) and up to 3 **offerings**, the key evidence of that death taken from the redacted séance evidence with its `[E#]` at the time: how it died (cause and exit code), its last words (the first key log line, or the last line), the one change between two lives when a release broke it, and then by cause the memory limit, the image that could not be pulled or the first Warning event. When the enshrined Pod dies again after its postmortem (each new death time counts once), or another Pod of its family dies after it (each Pod counts once), the list records a **return** in the graveyard file. The row then shows **🕯️ 第 N 次归来** (return #N) linked to the photo, the summoning bell gives a separate notice instead of "died again" and rings twice, the photo gets one small candle per return (at most 7), and the photo page warns that the earlier fix may not have worked and lists the offerings. Generating the postmortem again resets the returns. Sample mode never records a return, because the fixtures died before any burial. The JSON list is at `/api/graveyard`, with `family`, `offerings`, `returns` and `returnLog`, and `GET /api/spirits` includes each spirit's `family`. The file defaults to `~/.k8s-seance/graveyard.json` (directory mode 0700, file mode 0600, because postmortems contain log lines). Use `-graveyard <path>` to move it, or `-graveyard ""` to turn the graveyard off. Fixture mode writes tombstones too.

The web UI is in Chinese or English (see [English path](#english-path)). The recorded fixture spirits speak Chinese in a Chinese séance and English in an English séance. Postmortems are always in English.

## Quick start

Requires Go 1.26 or newer (the `go` directive in `go.mod` is `go 1.26.0`). No cluster and no API key are needed.

```sh
go run ./cmd/seance --fixtures --replay
```

Open the address shown in the log line `listening on http://...`. The default is <http://localhost:6660>. The root URL redirects to the spirit list, which shows three spirits:

| Pod | Cause |
| --- | --- |
| `memory-glutton-7cc6df9966-bf87c` | OOMKilled |
| `restless-spirit-84fc8b7c4c-52nl8` | CrashLoopBackOff |
| `lost-soul-5ff5cb8464-hjtms` | ImagePullBackOff |

Click **召唤** (summon) on any row. The opening streams in from the recording. Then ask one of the recorded questions (see [Sample mode](#sample-mode)), and click the postmortem button to see the report.

Stop the server with Ctrl+C.

## Sample mode

- `--fixtures` shows the three spirits embedded from `fixtures/*.json`. It never loads a kubeconfig or contacts a cluster.
- `--replay` returns LLM replies from `fixtures/replay/*.json`. Those replies were recorded from a real OpenAI-compatible model. Replay makes no network calls.
  - The opening (turn 0) and the postmortem (turn 100) match by turn index.
  - A follow-up matches a recorded question after normalization: surrounding spaces are trimmed, full-width `？！，。` count as `?!,.`, trailing `?!.,` and spaces are dropped, and ASCII letters ignore case.
  - A question with no recording, or a pod with no recording, gets the built-in mock reply.
- `--mock` returns the built-in template replies and makes no API calls. It cannot be combined with `--replay`.

The recorded questions, read from each file's `lastUser` field:

| Spirit | Recorded follow-ups | Also recorded |
| --- | --- | --- |
| `memory-glutton-7cc6df9966-bf87c` | 怎么修复？ (How do I fix it?) | opening, postmortem |
| `restless-spirit-84fc8b7c4c-52nl8` | 谁删了你？ (Who deleted you?) | opening, postmortem |
| `lost-soul-5ff5cb8464-hjtms` | 怎么修复？ (How do I fix it?) | opening, postmortem |

The opening is requested with the internal message `请做开场陈述。` ("Please give your opening statement"). It is not typed by the user.

English séances read `fixtures/replay/<pod>-en.json`. Each of the three spirits has a recorded English opening there, requested with `Please give your opening statement.`, and nothing else. English follow-ups get the English mock reply. The postmortem reuses the turn-100 recording in `<pod>.json`, which is already in English.

## English path

- **Interface language.** The first match wins: `?lang=zh` or `?lang=en` (the switch link in the top bar, which also sets the cookie `seance_lang`), the `seance_lang` cookie, the `-lang zh|en` flag, then the browser's `Accept-Language`. When that header lists `en` before `zh`, or lists neither, the UI is English. Without the header, the UI is Chinese.
- **Séance language.** A séance takes the interface language at the moment you click **Summon** (the form sends `lang`, and the JSON API accepts `"lang":"zh"|"en"`). It does not change afterwards. An English séance sends an English opening request, adds an English language rule to the system prompt, uses English quick questions, an English mock and an English exhausted line, and replays `<pod>-en.json`. Follow-ups are still answered in the language of the question. The Chinese system prompt is unchanged.
- If the interface language differs from the séance language, the séance page says that switching the language only changes the interface.

```sh
go run ./cmd/seance --fixtures --replay -lang en
```

To record the English openings again, delete `fixtures/replay/<pod>-en.json` and run `./demo/record-english.sh` with `SEANCE_LLM_BASE_URL`, `SEANCE_LLM_API_KEY` and `SEANCE_LLM_MODEL` set. It starts a temporary server with `--fixtures` and `SEANCE_RECORD=1`, makes at most one call per spirit (only openings that are not recorded yet), stops the server and reports how often the key occurs in the files it wrote, which must be 0.

The same modes work on the command line. This prints the autopsy of each fixture spirit, with its evidence list and opening, to stdout:

```sh
go run ./cmd/seance summon --fixtures
```

## Kind demo

This uses a local kind cluster with three Pods that die in different ways. You need Docker, `kind` and `kubectl`.

```sh
kind create cluster --name seance --config demo/kind-config.yaml
./demo/kill-them-all.sh
go run ./cmd/seance -n seance-demo
```

- `kind create cluster` builds a single-node cluster named `seance`. The kubectl context becomes `kind-seance`.
- `./demo/kill-them-all.sh` creates the namespace `seance-demo` and applies four manifests from `demo/`: `oom.yaml` (`python:3.12-alpine` holding 100 MB under a 64Mi limit), `crashloop.yaml` (`busybox`, exits with code 1 while `/etc/app/config.yaml` is missing), `imagepull.yaml` (a tag that does not exist) and `second-life.yaml` (a healthy `busybox` Deployment with 2 replicas and `MODE=normal`). It waits up to 120 seconds for `second-life` to roll out, then releases a bad version with `kubectl set env deployment/second-life MODE=turbo`. The new Pod exits with code 1, and `maxUnavailable: 0` keeps both old Pods alive. It then waits up to 120 seconds for OOMKilled, CrashLoopBackOff, ImagePullBackOff and the crash-looping `second-life` Pod to appear.
- `go run ./cmd/seance -n seance-demo` serves the UI for that namespace. Without an LLM configuration, the spirits use the mock automatically and nothing is printed about it. Add `--mock` to make that explicit.

The `summon` subcommand prints the same information without the web UI:

```sh
go run ./cmd/seance summon -n seance-demo
go run ./cmd/seance summon -n seance-demo -pod <pod-name>
```

Its flags are `-n`, `-kubeconfig`, `-pod`, `-tail`, `-fixtures`, and `-replay` or `-mock`. The `summon` path does not use the per-séance call budget or the opening cache (see [Cost notes](#cost-notes)).

Clean up:

```sh
./demo/kill-them-all.sh --cleanup
kind delete cluster --name seance
```

## Read-only RBAC

`demo/rbac-readonly.yaml` defines:

- A ClusterRole `k8s-seance-readonly` with `get`, `list` and `watch` on `pods`, `pods/log` and `events` in the core API group. Nothing else.
- A ServiceAccount `k8s-seance` in namespace `seance-demo`.
- A ClusterRoleBinding `k8s-seance-readonly` that attaches the role to that ServiceAccount. Because it is a ClusterRoleBinding, the ServiceAccount can read those resources in every namespace, not only `seance-demo`.

Apply it after the namespace exists (`kill-them-all.sh` creates it):

```sh
kubectl apply -f demo/rbac-readonly.yaml
```

Check the permissions. The expected answers are in the file's comments:

```sh
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance list pods -A        # yes
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance get pods/log -A     # yes
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance list events -A      # yes
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance delete pods -A      # no
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance create pods -A      # no
kubectl auth can-i --as=system:serviceaccount:seance-demo:k8s-seance get secrets -A      # no
```

To run K8s Séance as that ServiceAccount, without changing your default kubeconfig, write a separate kubeconfig in `/tmp`:

```sh
kubectl config view --minify --raw > /tmp/seance-ro.kubeconfig
kubectl --kubeconfig /tmp/seance-ro.kubeconfig config set-credentials seance-ro --token="$(kubectl create token k8s-seance -n seance-demo)"
kubectl --kubeconfig /tmp/seance-ro.kubeconfig config set-context --current --user=seance-ro
go run ./cmd/seance -n seance-demo --kubeconfig /tmp/seance-ro.kubeconfig
```

The copied file still holds the admin credentials of your original context, and the token is short-lived (`kubectl create token` defaults to one hour). Restrict the file while you use it and delete it afterwards:

```sh
chmod 600 /tmp/seance-ro.kubeconfig
# when finished
rm /tmp/seance-ro.kubeconfig
```

Outside `internal/rite` the code makes no write calls. In `internal/k8s` the only API calls are `Pods(ns).List`, `Pods(ns).Get`, `Pods(ns).Watch`, `Pods(ns).GetLogs` and `Events(ns).List`. `internal/readonly_test.go` scans `internal/` for write methods (Create, Update, Patch, Delete, Evict) and fails on any of them outside `internal/rite`. It also fails if any package other than `internal/web` imports `internal/rite`.

To remove the RBAC objects:

```sh
kubectl delete -f demo/rbac-readonly.yaml
```

## Casting (`--allow-resurrect`)

Casting writes to the cluster. Use it only on a demo cluster such as the kind demo above.

```sh
kubectl apply -f demo/rbac-readonly.yaml -f demo/rbac-resurrect.yaml
go run ./cmd/seance -n seance-demo --allow-resurrect
```

- **Off unless asked.** Without `--allow-resurrect` the spell panel says casting is off, and `POST /api/seances/{id}/cast` and `GET /api/seances/{id}/vitals` answer 403. In sample mode the panel says the spirit comes from fixtures, and the cast answers 403 too. `--allow-resurrect` with `--fixtures`, or without a cluster connection, logs a warning and leaves casting off.
- **Where.** Only in the namespaces of `-resurrect-ns` (comma-separated, default `seance-demo`). The server refuses to start with `--allow-resurrect` unless `-addr` is a loopback address (`localhost`, `127.0.0.1`, `[::1]`).
- **What.** Only the target controller of the spell, and only a Deployment, StatefulSet or DaemonSet. Castable spells: `patch` (memory limit; probe `timeoutSeconds` and `failureThreshold` that you enter, 1–300 and 1–100), `set-image` (a tag you type; it must match the image tag grammar and differ from the current tag), `rollback` (Deployments only: the Pod template of the ReplicaSet at the living revision replaces the current one with a JSON Patch, which fails if the Deployment changed since it was read) and `configmap-mount` (the pasted content, at most 64 KiB, goes into a ConfigMap labelled `app.kubernetes.io/managed-by=k8s-seance`; an existing ConfigMap without that label is never overwritten). Spells that need a Secret, a debugging session or a bare Pod or Job stay read-only.
- **How.** **试咒** (rehearse) sends every write with `dryRun=All`: the API server validates it and stores nothing. **施法** (cast) is accepted only within 10 minutes after a passing rehearsal with the same input, and only with the confirmation box ticked. Requests from another site (`Origin` of another host, or `Sec-Fetch-Site: cross-site`/`same-site`) are refused. Every write uses the field manager `k8s-seance`. The result lists each API request.
- **Did it come back?** After a real cast the page asks `GET /api/seances/{id}/vitals?since=<unix seconds>` every 3 seconds for up to 4 minutes. That is one read-only Pod list. Revived: a Pod of the family has been running and ready for 20 seconds and no Pod is failing. Fell again: a Pod created after the cast is failing or has restarted. Otherwise waiting; a failing Pod from before the cast only keeps it waiting until it is replaced. When revived, a spirit on the altar gets `revivedAt` and `revivedBy` in `graveyard.json` and `/api/graveyard`, and a **🌅 已复活** mark on its photo.
- **Permissions.** `demo/rbac-resurrect.yaml` is a Role and RoleBinding in `seance-demo` only, for the `k8s-seance` ServiceAccount: `get`/`patch` on deployments, statefulsets and daemonsets, `get`/`list` on replicasets, and `get`/`create`/`update` on configmaps. No delete and no Secrets. The expected `kubectl auth can-i` answers are in the file's comments.

## Cost notes

Limits come from `internal/llm/budget.go`. They apply to each séance created in the web UI.

- **Calls:** 6 per séance. The opening, each follow-up and each postmortem generation counts as one call, including failed calls. Mock and replay calls count too.
- **Input:** at most 6000 tokens per call. The size is estimated as half the rune count of the message and evidence text, not with a real tokenizer. When a request is too large, the oldest history is dropped first. The last user message and the evidence are never dropped. If the request still does not fit, the call is refused without being counted, and the UI shows an error.
- **Output:** 400 tokens per opening or follow-up call, and 1500 tokens per postmortem call. These are sent as `max_tokens`.
- **Total input:** at most 30000 estimated tokens per séance, summed over all calls (the per-call estimate, or the provider's reported input tokens when they are larger). When a request fits the per-call 6000 but not what is left of the 30000, older history is dropped first. If it still does not fit, the séance is treated as out of calls: the ghost says the exhausted line, and the postmortem uses the rule template with `budget_exhausted`. Override with `SEANCE_BUDGET_MAX_TOTAL_INPUT_TOKENS` (a positive integer; other values are ignored).
- **When the calls run out:** the next question gets the fixed line `仪式之力已耗尽，亡灵沉入黑暗……`, which means "the ritual power is spent and the ghost sinks into darkness", and no call is made. The postmortem then comes from the rule template, with the reason `budget_exhausted` shown on the page.
- **Postmortem cost:** the postmortem button generates a new report and spends one call each time. The `.md` download reuses the stored report if one exists.
- **Override:** `SEANCE_BUDGET_MAX_CALLS` takes a positive integer for the call limit. Other values are ignored. There is no environment variable for the per-call token limits. The web server reads these variables. The `summon` path does not.
- **Opening cache:** kept in memory by the running server. The key is the Pod UID, the highest container restart count, and the model name. Only successful openings are stored. A cache hit makes no call. Restarting the server clears the cache.
- **Trimming:** the collector keeps the last 200 log lines per container. Evidence text is capped at 24000 characters in total. Chat sends the last 4 rounds of history. The postmortem sends the last 8 transcript turns, each cut at 200 runes.
- **Séance lifetime:** a séance is kept in memory and expires after 2 hours without use.

## Configure a real LLM

K8s Séance works with any OpenAI-compatible Chat Completions endpoint that supports streaming. It needs three settings: a base URL, an API key and a model name. There are no vendor defaults. The client posts to `{base_url}/chat/completions`.

Environment variables:

```sh
export SEANCE_LLM_BASE_URL="https://<your-endpoint-host>/v1"
export SEANCE_LLM_API_KEY="<your-api-key>"
export SEANCE_LLM_MODEL="<your-model-name>"
go run ./cmd/seance -n seance-demo
```

Or the config file `~/.k8s-seance/config.yaml`:

```yaml
llm:
  base_url: "https://<your-endpoint-host>/v1"
  api_key: "<your-api-key>"
  model: "<your-model-name>"
```

Only this shape is read: a top-level `llm:` mapping with those three keys. Quotes and `#` comments are allowed. Run `chmod 600 ~/.k8s-seance/config.yaml` to keep the key readable only by you.

Rules:

- An environment variable overrides the file, one field at a time.
- `SEANCE_LLM_MODE` can be `http`, `replay` or `mock`. The flags `--replay` and `--mock` do the same. Without a mode, the app uses `http` when all three settings are present, and `mock` otherwise.
- If the mode is `http` but a setting is missing, the app prints a notice and uses the mock.

Recording: with `SEANCE_RECORD=1` and a real endpoint, the web UI saves each new turn into `fixtures/replay/<pod>.json`. Run this from the repository root, because the path is relative. Turns that are already recorded are replayed without an API call, so only missing turns reach the endpoint. Empty replies, failed calls and mock replies are not saved. Each saved turn holds the cause, the turn index, the question text, the reply and the token counts. Headers and the API key are not saved. See `fixtures/replay/README.md` for the format.

Warnings:

- Never commit an API key. Keep it in the environment or in `~/.k8s-seance/config.yaml`, outside the repository.
- Review recordings before you commit them. Questions are stored as typed.
- `SEANCE_RECORD=1 go run ./cmd/seance summon ...` also writes each collected Pod to `fixtures/oom.json`, `fixtures/crashloop.json` or `fixtures/imagepull.json`, or to `$SEANCE_FIXTURES_DIR` if that is set. Those files contain the full autopsy, including container log tails. Redaction applies only to the evidence sent to the LLM, not to these files. Check them for secrets before you commit.

## How we used Kiro

The repository was built with Kiro as a spec-driven project. Everything below comes from files in `.kiro/` and from the git history.

- **Specs** in `.kiro/specs/k8s-seance/`:
  - `requirements.md` has 9 numbered requirements with acceptance criteria. Requirement 9 (the graveyard) is marked optional.
  - `design.md` covers the architecture, the component interfaces, the data models, error handling and the testing strategy.
  - `tasks.md` is the implementation plan: 9 top-level tasks with 27 subtasks, plus optional task 8.
- **Steering** in `.kiro/steering/`:
  - `product.md`: the product in one line, its principles (read-only, low cost, usable offline) and what it does not do.
  - `tech.md`: the stack and its limits (Go, `net/http`, htmx, client-go, one YAML library) and the coding tool and model used (Kiro CLI with claude-haiku-5.5).
  - `structure.md`: the directory layout and conventions, such as the collector emitting one `PodAutopsy` type and new features being specified before they are coded.
- **Tasks to commits.** Most commit subjects name the task they implement, for example:
  - `c8d6a29 docs: add K8s Seance spec, Kiro specs and steering` (the specs themselves)
  - `f41809b chore: scaffold Go module, directory layout, MIT LICENSE and README (task 1.1)`
  - `2e9cb5d feat(llm): per-seance budget, opening cache, history trim (4.3)`
  - `181ab36 feat(llm): replay matches follow-ups by normalised question; opening/postmortem by index`
  - `e1b9855 feat(web): postmortem panel with copy and .md download (7.3)`
  - `6eecc2b chore(tasks): 4.5 real-LLM recordings done`

  The history has 35 commits, all dated 2026-11-01.
- **Progress** is tracked with checkboxes in `tasks.md`: 38 checkboxes, 28 checked and 10 open. The open ones are task 1 and subtask 1.1 (its child item, the Devpost identity verification, is also open), task 4 (its header is still open even though all of 4.1 to 4.5 are checked), task 8 and its subtasks 8.1 and 8.2 (optional), and task 9 with subtasks 9.1 and 9.2.

## Project layout

```
cmd/seance/          CLI entry point: web server and the summon subcommand
internal/
  k8s/               read-only collector (client-go): Pods List/Get/Watch/GetLogs, Events List; family (lineage.go); vitals after a cast (vitals.go)
  resurrect/         rule-based resurrection spell and its castable Action (no LLM, no cluster)
  rite/              the only package that writes to the cluster, used only with --allow-resurrect
  autopsy/           PodAutopsy model, death-cause classifier, evidence builder
  spirit/            persona prompts (go:embed), séance session, chat
  postmortem/        Markdown postmortem generator and template
  llm/               Provider interface, HTTP client, mock, replay, recorder, budget, config
  graveyard/         local tombstone file (graveyard.json): one tombstone per Pod with its postmortem
  web/               net/http routes, SSE stream, html/template views, static assets
fixtures/            embedded offline spirits (*.json) and fixtures/replay/ recorded replies
demo/                kind config, four death manifests, kill-them-all.sh, read-only and casting RBAC
docs/                notes and screenshots (placeholder)
.kiro/               specs and steering
```

Tests run with `go test ./...`.

## License

MIT. Copyright (c) 2026 juji zhang. See [LICENSE](LICENSE).
