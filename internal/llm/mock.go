package llm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
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

// mockText builds the full reply. It always cites evidence when any exists.
func mockText(req ChatRequest) string {
	followUp := userTurns(req.Messages) > 1
	picked := pickEvidence(req.Cause, req.Evidence)

	if len(picked) == 0 {
		if followUp {
			return noRecord
		}
		return voiceFor(req.Cause) + noRecord
	}

	if followUp {
		// A follow-up gets a short answer that cites only the strongest item.
		return followUpLine + citation(picked[:1]) + "。"
	}
	return voiceFor(req.Cause) + "证据在此：" + citation(picked) + "。"
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

// citation renders each item as 「fragment」[E#], joined by commas.
func citation(items []autopsy.Evidence) string {
	parts := make([]string, 0, len(items))
	for _, e := range items {
		parts = append(parts, "「"+fragment(e)+"」["+e.ID+"]")
	}
	return strings.Join(parts, "，")
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
