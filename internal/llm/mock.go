package llm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

const (
	// noRecord is the fixed answer when there is no evidence to cite.
	noRecord = "冥界没有留下记录。"
	// followUpLine opens every answer to a follow-up question.
	followUpLine = "我已经把能说的都说了。"
	// maxCitations caps how many evidence items one mock line cites.
	maxCitations = 3
	// maxQuoteRunes caps the length of each quoted evidence fragment.
	maxQuoteRunes = 60
	// chunkRunes is the size of each streamed fragment.
	chunkRunes = 4
)

// importantLog matches log lines worth citing: errors, fatal messages and OOM.
var importantLog = regexp.MustCompile(`(?i)error|fatal|oom|panic|killed`)

// logLabel matches the "container X current log: " or "container X previous log: " prefix.
var logLabel = regexp.MustCompile(`^container \S+ (?:current|previous) log: `)

// voiceLines holds the per-cause opening line of the ghost.
var voiceLines = map[autopsy.CauseOfDeath]string{
	autopsy.CauseOOMKilled:        "我的内存一点点涨满，窒息的时候，我越来越重。",
	autopsy.CauseCrashLoopBackOff: "我被一次次拉回人间，一次次复活又倒下，像一场没人喊停的葬礼。",
	autopsy.CauseImagePullBackOff: "我从未真正活过，镜像拉不下来，我连出生证明都没有。",
	autopsy.CauseProbeFailure:     "他们以为我死了，于是真的杀了我。探针每敲一次门，我都答不上来。",
	autopsy.CauseError:            "我的进程在不该停的地方停了下来。",
	autopsy.CauseUnknown:          "冥界的记录有些模糊，我也说不清自己是怎么死的。",
}

// voiceLinesEN is voiceLines for an English séance.
var voiceLinesEN = map[autopsy.CauseOfDeath]string{
	autopsy.CauseOOMKilled:        "My memory filled up bit by bit. As I suffocated, I grew heavier and heavier.",
	autopsy.CauseCrashLoopBackOff: "They keep dragging me back to the world. I rise and fall again and again, a funeral nobody will stop.",
	autopsy.CauseImagePullBackOff: "I never truly lived. My image could not be pulled, so I never even got a birth certificate.",
	autopsy.CauseProbeFailure:     "They thought I was dead, so they really killed me. Each time the probe knocked, I could not answer.",
	autopsy.CauseError:            "My process stopped where it should not have stopped.",
	autopsy.CauseUnknown:          "The records of the underworld are blurred. I cannot say how I died.",
}

// MockProvider builds a fixed line from the cause and the evidence, then streams it.
// Delay is the pause after each fragment. Zero means no sleep.
type MockProvider struct {
	Delay time.Duration
}

var _ Provider = MockProvider{}

// Stream implements Provider. It returns the context error when ctx is done,
// and the error from onDelta when onDelta fails.
func (m MockProvider) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if onDelta == nil {
		return Usage{}, errors.New("mock: nil onDelta")
	}
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}

	text := mockText(req)
	usage := Usage{
		InputTokens:  inputRunes(req),
		OutputTokens: utf8.RuneCountInString(text),
	}

	runes := []rune(text)
	for start := 0; start < len(runes); start += chunkRunes {
		if err := ctx.Err(); err != nil {
			return Usage{}, err
		}
		end := min(start+chunkRunes, len(runes))
		if err := onDelta(string(runes[start:end])); err != nil {
			return Usage{}, err
		}
		if m.Delay > 0 {
			timer := time.NewTimer(m.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return Usage{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return usage, nil
}

// mockText builds the full reply for the request's purpose. Every reply that
// has evidence to cite carries at least one [E#].
func mockText(req ChatRequest) string {
	switch req.Purpose {
	case PurposeOpening:
		return openingText(req)
	case PurposeChat:
		return chatText(req)
	case PurposePostmortem:
		return postmortemText(req)
	}
	// Purpose not set, so this is an older caller. More than one user message
	// means a follow-up, which keeps the original short answer.
	if userTurns(req.Messages) > 1 {
		return legacyFollowUpText(req)
	}
	return openingText(req)
}

// openingText is the ghost's first line: the voice, the time of death when it is
// known, and the strongest evidence. The language follows req.Lang.
func openingText(req ChatRequest) string {
	picked := pickEvidence(req.Cause, req.Evidence)
	if langOrDefault(req.Lang) == LangEN {
		return openingTextEN(req, picked)
	}
	death := deathLine(req.DeathTime)
	if len(picked) == 0 {
		return voiceFor(req.Cause) + death + noRecord + formerLifeLine(req.Evidence, false)
	}
	return voiceFor(req.Cause) + death + "证据在此：" + citation(picked, false) + "。" + formerLifeLine(req.Evidence, false)
}

// livingFormerLife matches the lineage evidence of an older revision with at
// least one living Pod.
var livingFormerLife = regexp.MustCompile(`^revision \S+ \(older, former life\): alive [1-9]\d*/`)

// formerLifeLine is the opening sentence about a former life that is still
// alive (Requirement 16.5): when the family verdict is regression, the ghost
// says that the release broke it and cites the living revision. Otherwise "".
func formerLifeLine(ev []autopsy.Evidence, english bool) string {
	regression := false
	for _, e := range ev {
		if e.Kind == "lineage" && strings.Contains(e.Text, "verdict="+autopsy.VerdictRegression+" ") {
			regression = true
		}
	}
	if !regression {
		return ""
	}
	for _, e := range ev {
		if e.Kind != "lineage" || !livingFormerLife.MatchString(e.Text) {
			continue
		}
		if english {
			return " My former life is still alive: " + citation([]autopsy.Evidence{e}, true) + ". It was this release that broke me, so roll back first."
		}
		return "我的前世还活着：" + citation([]autopsy.Evidence{e}, false) + "。是这次发布把我改坏的，先回滚。"
	}
	return ""
}

// openingTextEN is openingText for an English séance. It uses no CJK punctuation.
func openingTextEN(req ChatRequest, picked []autopsy.Evidence) string {
	s := voiceForEN(req.Cause)
	if req.DeathTime != "" {
		s += " I died at " + req.DeathTime + "."
	}
	if len(picked) == 0 {
		return s + " " + recordLine(true) + formerLifeLine(req.Evidence, true)
	}
	return s + " The evidence: " + citation(picked, true) + "." + formerLifeLine(req.Evidence, true)
}

func voiceForEN(cause autopsy.CauseOfDeath) string {
	if v, ok := voiceLinesEN[cause]; ok {
		return v
	}
	return voiceLinesEN[autopsy.CauseUnknown]
}

// deathLine returns the time-of-death sentence, or "" when the time is unknown.
func deathLine(t string) string {
	if t == "" {
		return ""
	}
	return "我死于 " + t + "。"
}

// legacyFollowUpText is the answer used when Purpose is not set. It cites only the
// strongest item.
func legacyFollowUpText(req ChatRequest) string {
	picked := pickEvidence(req.Cause, req.Evidence)
	if len(picked) == 0 {
		return noRecord
	}
	return followUpLine + citation(picked[:1], false) + "。"
}

// Keyword lists for chatText. Matching is substring based, on the lower-cased
// question. Fix words are checked first, so a question like "怎么修内存" is a fix
// question.
var (
	fixKeywords   = []string{"修复", "怎么修", "怎么办", "解决", "避免", "fix", "solve", "prevent", "how do i", "what should"}
	causeKeywords = []string{"怎么死", "死因", "为什么", "日志", "证据", "内存", "镜像", "什么时候",
		"how did you die", "why", "cause", "log", "evidence", "memory", "image", "when"}
	timeKeywords = []string{"什么时候", "几点", "when", "what time"}
)

// fixAdvice is the cause-matched suggestion for a fix question, in Chinese and English.
// The text is a suggestion only. The mock never claims to have run a command.
type fixAdvice struct{ zh, en string }

var fixAdvices = map[autopsy.CauseOfDeath]fixAdvice{
	autopsy.CauseOOMKilled: {
		zh: "可以考虑把内存上限（memory limit）调到高于实际峰值，并排查内存增长点（缓存、无界队列、泄漏）。",
		en: "Consider raising the memory limit above the real peak usage, and look for what keeps growing (caches, unbounded queues, leaks).",
	},
	autopsy.CauseCrashLoopBackOff: {
		zh: "建议先看上一次容器的日志（kubectl logs --previous），修正启动命令、入口参数或配置文件路径。",
		en: "Read the previous container's log (kubectl logs --previous), then fix the startup command, entrypoint arguments or config file path.",
	},
	autopsy.CauseError: {
		zh: "建议先看上一次容器的日志（kubectl logs --previous），修正启动命令、入口参数或配置文件路径。",
		en: "Read the previous container's log (kubectl logs --previous), then fix the startup command, entrypoint arguments or config file path.",
	},
	autopsy.CauseImagePullBackOff: {
		zh: "建议确认镜像名和 tag 真实存在，检查仓库地址与访问权限，必要时配置 imagePullSecrets。",
		en: "Check that the image name and tag really exist, check the registry address and access rights, and set imagePullSecrets if the registry is private.",
	},
	autopsy.CauseProbeFailure: {
		zh: "建议确认探针的路径和端口能正常响应，并适当调大 initialDelaySeconds 或 timeoutSeconds。",
		en: "Check that the probe path and port respond, and consider a larger initialDelaySeconds or timeoutSeconds.",
	},
	autopsy.CauseUnknown: {
		zh: "建议先运行 kubectl describe pod 和 kubectl logs --previous 补充信息。",
		en: "Run kubectl describe pod and kubectl logs --previous for more information.",
	},
}

// chatText answers a follow-up question. The language follows the question: a
// question with no CJK characters gets an English answer.
func chatText(req ChatRequest) string {
	question := lastUserContent(req.Messages)
	english := !hasCJK(question)
	lower := strings.ToLower(question)
	picked := pickEvidence(req.Cause, req.Evidence)

	if len(picked) == 0 {
		return recordLine(english)
	}
	switch {
	case containsAny(lower, fixKeywords):
		advice := adviceFor(req.Cause)
		if english {
			return advice.en + " Evidence: " + citation(picked[:1], true) + "."
		}
		return advice.zh + "依据：" + citation(picked[:1], false) + "。"
	case containsAny(lower, causeKeywords):
		return causeReply(req, picked, english, containsAny(lower, timeKeywords))
	default:
		return recordLine(english) + otherEvidence(picked, english)
	}
}

// causeReply restates the cause with up to two citations. It adds the time of
// death when the question asks for it and the time is known.
func causeReply(req ChatRequest, picked []autopsy.Evidence, english, askedWhen bool) string {
	cite := citation(picked[:min(2, len(picked))], english)
	if english {
		s := "My cause of death is " + string(req.Cause) + ". Evidence: " + cite + "."
		if askedWhen && req.DeathTime != "" {
			s = "I died at " + req.DeathTime + ". " + s
		}
		return s
	}
	s := "我的死因是 " + string(req.Cause) + "。证据：" + cite + "。"
	if askedWhen && req.DeathTime != "" {
		s = "我死于 " + req.DeathTime + "。" + s
	}
	return s
}

// otherEvidence is the single citation added to a question the evidence cannot
// answer. It keeps the reply citing the cause evidence.
func otherEvidence(picked []autopsy.Evidence, english bool) string {
	if english {
		return " The only thing I know: " + citation(picked[:1], true) + "."
	}
	return "我只知道这一件事：" + citation(picked[:1], false) + "。"
}

// recordLine is the fixed "no record" answer in the given language.
func recordLine(english bool) string {
	if english {
		return "The underworld kept no record of that."
	}
	return noRecord
}

// adviceFor returns the fix advice for a cause. Causes without an entry get the
// unknown advice.
func adviceFor(cause autopsy.CauseOfDeath) fixAdvice {
	if a, ok := fixAdvices[cause]; ok {
		return a
	}
	return fixAdvices[autopsy.CauseUnknown]
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// hasCJK reports whether s has a Han, Hiragana, Katakana or Hangul character.
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// pickEvidence chooses up to maxCitations items. Passes run in an order that
// depends on the cause, and each pass stops once the limit is reached:
//
//	ImagePullBackOff: status, spec image, Warning events
//	OOMKilled:        status, spec limits, important logs, Warning events
//	other:            status, important logs, Warning events
//
// A log line whose text was already cited is skipped, so a previous log that
// repeats the current log is not cited twice.
func pickEvidence(cause autopsy.CauseOfDeath, ev []autopsy.Evidence) []autopsy.Evidence {
	status := func(e autopsy.Evidence) bool { return e.Kind == "status" }
	image := func(e autopsy.Evidence) bool {
		return e.Kind == "spec" && strings.Contains(e.Text, " image: ")
	}
	limits := func(e autopsy.Evidence) bool {
		return e.Kind == "spec" && strings.Contains(e.Text, " limits: ") && !strings.Contains(e.Text, "limits: none")
	}
	logs := func(e autopsy.Evidence) bool {
		return e.Kind == "log" && importantLog.MatchString(logBody(e.Text))
	}
	warnings := func(e autopsy.Evidence) bool {
		return e.Kind == "event" && strings.Contains(e.Text, " Warning ")
	}

	var passes []func(autopsy.Evidence) bool
	switch cause {
	case autopsy.CauseImagePullBackOff:
		passes = []func(autopsy.Evidence) bool{status, image, warnings}
	case autopsy.CauseOOMKilled:
		passes = []func(autopsy.Evidence) bool{status, limits, logs, warnings}
	default:
		passes = []func(autopsy.Evidence) bool{status, logs, warnings}
	}

	var out []autopsy.Evidence
	seenLog := make(map[string]bool)
	for _, match := range passes {
		for _, e := range ev {
			if len(out) == maxCitations {
				return out
			}
			if !match(e) {
				continue
			}
			if e.Kind == "log" {
				body := logBody(e.Text)
				if seenLog[body] {
					continue
				}
				seenLog[body] = true
			}
			out = append(out, e)
		}
	}
	return out
}

// citation renders each item as 「fragment」[E#] joined by "，" in Chinese. English
// replies use "fragment"[E#] joined by ", ", so no CJK punctuation appears.
// Inner double quotes in an English fragment become single quotes, so the
// straight quotes around the fragment stay unambiguous.
func citation(items []autopsy.Evidence, english bool) string {
	left, right, sep := "「", "」", "，"
	if english {
		left, right, sep = `"`, `"`, ", "
	}
	parts := make([]string, 0, len(items))
	for _, e := range items {
		frag := fragment(e)
		if english {
			frag = strings.ReplaceAll(frag, `"`, "'")
		}
		parts = append(parts, left+frag+right+"["+e.ID+"]")
	}
	return strings.Join(parts, sep)
}

// fragment picks the telling part of one evidence item and caps it at maxQuoteRunes.
// Status items quote from "lastState: terminated" or "reason=" onward. Log items
// quote the log line without its label. Other items quote their whole text.
func fragment(e autopsy.Evidence) string {
	switch e.Kind {
	case "status":
		if i := strings.Index(e.Text, "lastState: terminated"); i >= 0 {
			return quote(e.Text[i:])
		}
		if i := strings.Index(e.Text, "reason="); i >= 0 {
			return quote(e.Text[i:])
		}
	case "log":
		return quote(logBody(e.Text))
	}
	return quote(e.Text)
}

// logBody strips the "container X current log: " or "previous log: " label.
// Text without the label is returned unchanged.
func logBody(text string) string {
	return logLabel.ReplaceAllString(text, "")
}

// quote shortens text to maxQuoteRunes runes, adding an ellipsis when it cuts.
func quote(text string) string {
	runes := []rune(text)
	if len(runes) <= maxQuoteRunes {
		return text
	}
	return string(runes[:maxQuoteRunes]) + "…"
}

func voiceFor(cause autopsy.CauseOfDeath) string {
	if v, ok := voiceLines[cause]; ok {
		return v
	}
	return voiceLines[autopsy.CauseUnknown]
}

func userTurns(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "user" {
			n++
		}
	}
	return n
}

// inputRunes estimates input size as the rune count of messages and evidence.
func inputRunes(req ChatRequest) int {
	n := 0
	for _, m := range req.Messages {
		n += utf8.RuneCountInString(m.Content)
	}
	for _, e := range req.Evidence {
		n += utf8.RuneCountInString(e.Text)
	}
	return n
}

// pmText holds the four postmortem blocks for one cause. Each field is filled in
// by postmortemText, which adds the [E#] citations.
type pmText struct {
	summary, rootCause, remediation string
	actionItems                     []string
}

// postmortemText returns the four delimited blocks the postmortem generator
// parses: ### SUMMARY, ### ROOT_CAUSE, ### REMEDIATION and ### ACTION_ITEMS.
// The tone is formal, with no first-person spirit voice. Citations come from the
// same pickEvidence selection the other mock replies use.
func postmortemText(req ChatRequest) string {
	picked := pickEvidence(req.Cause, req.Evidence)
	var t pmText
	if len(picked) == 0 {
		t = pmText{
			summary:     "The recorded evidence does not show a single cause of failure.",
			rootCause:   "No evidence was recorded, so the root cause cannot be confirmed.",
			remediation: "- Run kubectl describe pod and kubectl logs --previous to collect more information.",
			actionItems: []string{
				"- [ ] Collect kubectl describe pod output and previous container logs for this Pod.",
				"- [ ] Confirm the root cause once the missing data is collected.",
				"- [ ] Keep event history long enough to review failures like this one.",
			},
		}
	} else {
		ref := func(i int) string {
			if i < len(picked) {
				return "[" + picked[i].ID + "]"
			}
			return ""
		}
		t = postmortemFor(req.Cause)(ref)
	}
	return "### SUMMARY\n" + t.summary + "\n\n" +
		"### ROOT_CAUSE\n" + t.rootCause + "\n\n" +
		"### REMEDIATION\n" + t.remediation + "\n\n" +
		"### ACTION_ITEMS\n" + strings.Join(t.actionItems, "\n") + "\n"
}

// postmortemFor returns the cause-specific block builder. ref(i) is the citation
// of the i-th picked evidence item, or "" when there is no such item.
func postmortemFor(cause autopsy.CauseOfDeath) func(ref func(int) string) pmText {
	switch cause {
	case autopsy.CauseOOMKilled:
		return func(ref func(int) string) pmText {
			return pmText{
				summary: "The container was terminated by the kernel out-of-memory killer (reason OOMKilled, exit code 137). " +
					"The Pod restarted repeatedly and entered CrashLoopBackOff. " + ref(0) + ref(1),
				rootCause: "Memory use of the container grew past its memory limit, so the kernel ended the process on each run. " +
					"The memory limit is below the working set of the workload. " + ref(0) + ref(1),
				remediation: "- Raise the memory limit above the observed peak usage, and set the memory request close to typical usage.\n" +
					"- Find what keeps memory growing: caches, unbounded queues or leaks. Profile the workload under load.\n" +
					"- After the change, check the container logs and events for new OOMKilled exits.",
				actionItems: []string{
					"- [ ] Raise the memory limit of the container above its observed peak usage. " + ref(1),
					"- [ ] Profile the workload under load and identify the allocation that keeps growing.",
					"- [ ] Add an alert on container memory usage near the limit, so the next OOM is seen before the restart.",
				},
			}
		}
	case autopsy.CauseCrashLoopBackOff, autopsy.CauseError:
		return func(ref func(int) string) pmText {
			return pmText{
				summary: "The container process exited with a non-zero exit code on each start. " +
					"The kubelet restarted it with increasing back-off, so the Pod is in CrashLoopBackOff. " + ref(0) + ref(1),
				rootCause: "The container process fails during startup. " +
					"The failure is recorded in the container log, so the startup command, arguments or configuration is the likely cause. " + ref(1),
				remediation: "- Read the previous container log (kubectl logs --previous) for the first error line.\n" +
					"- Correct the startup command, entrypoint arguments or config file path.\n" +
					"- Run the same image with the same command outside the cluster to confirm that it starts and stays up.",
				actionItems: []string{
					"- [ ] Correct the startup command or config path named in the previous container log. " + ref(1),
					"- [ ] Confirm that the config file or secret the process needs is mounted at the expected path.",
					"- [ ] Check the exit code and the previous log before each restart is retried.",
				},
			}
		}
	case autopsy.CauseImagePullBackOff:
		return func(ref func(int) string) pmText {
			return pmText{
				summary: "The container image could not be pulled, so the container never started and the Pod is in ImagePullBackOff. " +
					ref(0) + ref(1),
				rootCause: "The image reference does not resolve to a pullable image. " +
					"The image name, the tag or the registry access rights are wrong. " + ref(1) + ref(2),
				remediation: "- Confirm that the image name and tag exist in the registry.\n" +
					"- Check the registry address and access rights, and set imagePullSecrets for a private registry.\n" +
					"- Pull the image by hand on the node to confirm that the reference resolves.",
				actionItems: []string{
					"- [ ] Correct the image name or tag in the workload spec. " + ref(1),
					"- [ ] Confirm the registry credentials referenced by imagePullSecrets of the Pod.",
					"- [ ] Pin the image to a tag that exists in the registry, not a misspelled or moving tag.",
				},
			}
		}
	case autopsy.CauseProbeFailure:
		return func(ref func(int) string) pmText {
			return pmText{
				summary: "The kubelet restarted the container after failed probes. " + ref(0) + ref(1),
				rootCause: "The probe did not get a timely success response from the container. " +
					"The probe path, port or timing is wrong, or the application starts more slowly than the probe allows. " + ref(1),
				remediation: "- Check that the probe path and port respond from inside the container.\n" +
					"- Raise initialDelaySeconds or timeoutSeconds to match the measured start-up time.\n" +
					"- Confirm that the probe is needed, because a failing probe restarts an application that may be healthy.",
				actionItems: []string{
					"- [ ] Verify that the probe path and port return a success code. " + ref(1),
					"- [ ] Tune initialDelaySeconds and timeoutSeconds from the measured start-up time.",
					"- [ ] Review the failure threshold of the probe.",
				},
			}
		}
	default:
		return func(ref func(int) string) pmText {
			return pmText{
				summary: "The recorded evidence does not establish a single cause of failure. " + ref(0) + ref(1),
				rootCause: "The evidence is incomplete, so the root cause cannot be confirmed from the recorded data. " +
					ref(1),
				remediation: "- Run kubectl describe pod and kubectl logs --previous to collect more information.\n" +
					"- Compare the events and the container states with the time of the failure.",
				actionItems: []string{
					"- [ ] Collect kubectl describe pod output and previous container logs for this Pod. " + ref(0),
					"- [ ] Confirm the root cause once the missing data is collected.",
					"- [ ] Keep event history long enough to review failures like this one.",
				},
			}
		}
	}
}
