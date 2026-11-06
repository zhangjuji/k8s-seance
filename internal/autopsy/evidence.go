package autopsy

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultTailLines = 200
	defaultMaxChars  = 24000
	// keepLastLines is how many of the final log lines are always kept as evidence.
	keepLastLines = 5
	redacted      = "[REDACTED]"
)

// EvidenceOptions controls how much log and evidence text is packed.
// Zero or negative values fall back to the defaults (TailLines 200, MaxChars 24000).
type EvidenceOptions struct {
	TailLines int // log lines kept per log source, after collapsing duplicates
	MaxChars  int // maximum total length of all Evidence.Text values, in bytes
}

var (
	// secretKV matches key=value and key: value for common secret key names.
	// The key name is kept, only the value is replaced.
	secretKV = regexp.MustCompile(`(?i)((?:api_key|apikey|token|password|passwd|secret)\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"']+)`)
	// bearerToken matches "Bearer <x>" in any casing.
	bearerToken = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9\-._~+/]+=*`)
	// awsAccessKey matches the shape of an AWS access key ID.
	awsAccessKey = regexp.MustCompile(`AKIA[0-9A-Z]{16}`)
	// importantLine marks log lines that are kept even when they are not among the last lines.
	importantLine = regexp.MustCompile(`(?i)error|panic|exception|killed|fatal`)
)

// BuildEvidence turns a PodAutopsy into numbered evidence items E1..En.
// Items are ordered: status and spec per container, then events, then the
// family (lineage), then logs.
// Every Text is redacted. Once the total text would exceed MaxChars, no more
// items are added, so the numbering stays contiguous.
func BuildEvidence(a *PodAutopsy, opts EvidenceOptions) ([]Evidence, error) {
	if a == nil {
		return nil, errors.New("evidence: nil autopsy")
	}
	if opts.TailLines <= 0 {
		opts.TailLines = defaultTailLines
	}
	if opts.MaxChars <= 0 {
		opts.MaxChars = defaultMaxChars
	}

	b := &builder{items: make([]Evidence, 0), max: opts.MaxChars}

	for _, c := range a.Containers {
		b.add("status", containerStatusText(c), stateTime(c.State))
		b.add("spec", fmt.Sprintf("container %s image: %s", c.Name, c.Image), nil)
		b.add("spec", fmt.Sprintf("container %s limits: %s; requests: %s",
			c.Name, resourceText(c.Limits), resourceText(c.Requests)), nil)
	}
	for _, e := range a.Events {
		b.add("event", eventText(e), eventTime(e))
	}
	for _, text := range LineageTexts(a.Lineage) {
		b.add("lineage", text, nil)
	}
	for _, c := range a.Containers {
		addLogs(b, c, opts.TailLines)
	}

	return b.items, nil
}

// builder accumulates evidence items and enforces the character budget.
type builder struct {
	items []Evidence
	used  int
	max   int
	full  bool
}

func (b *builder) add(kind, text string, ts *time.Time) {
	if b.full {
		return
	}
	text = redact(text)
	if b.used+len(text) > b.max {
		b.full = true
		return
	}
	b.used += len(text)
	b.items = append(b.items, Evidence{
		ID:        fmt.Sprintf("E%d", len(b.items)+1),
		Kind:      kind,
		Text:      text,
		Timestamp: ts,
	})
}

// addLogs emits log evidence for one container: the current log, then the previous log.
func addLogs(b *builder, c ContainerAutopsy, tail int) {
	for _, line := range selectLogLines(c.LogTail, tail) {
		b.add("log", fmt.Sprintf("container %s current log: %s", c.Name, line), nil)
	}
	if c.PreviousLogTail == nil {
		// Req 2.5: a missing previous log is recorded, not treated as an error.
		b.add("log", fmt.Sprintf("container %s previous container logs: no record", c.Name), nil)
		return
	}
	for _, line := range selectLogLines(c.PreviousLogTail, tail) {
		b.add("log", fmt.Sprintf("container %s previous log: %s", c.Name, line), nil)
	}
}

// selectLogLines collapses consecutive duplicate lines, keeps the last tail lines,
// then returns the last keepLastLines lines plus earlier lines that look like errors.
func selectLogLines(lines []string, tail int) []string {
	kept := collapseDuplicates(lines)
	if len(kept) > tail {
		kept = kept[len(kept)-tail:]
	}

	n := len(kept)
	var out []string
	for i, line := range kept {
		if i >= n-keepLastLines || importantLine.MatchString(line) {
			out = append(out, line)
		}
	}
	return out
}

// collapseDuplicates drops a line when it equals the line directly before it.
func collapseDuplicates(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		if i > 0 && line == lines[i-1] {
			continue
		}
		out = append(out, line)
	}
	return out
}

// Redact replaces secret values in s with [REDACTED] and keeps key names
// visible. It is the redaction rule of every evidence text.
func Redact(s string) string { return redact(s) }

// redact replaces secret values with [REDACTED] and keeps the key name visible.
func redact(s string) string {
	s = secretKV.ReplaceAllString(s, "${1}"+redacted)
	s = bearerToken.ReplaceAllString(s, "${1}"+redacted)
	s = awsAccessKey.ReplaceAllString(s, redacted)
	return s
}

func containerStatusText(c ContainerAutopsy) string {
	last := "none"
	if c.LastState != nil {
		last = stateSummary(*c.LastState)
	}
	return fmt.Sprintf("container %s state: %s; restarts: %d; lastState: %s",
		c.Name, stateSummary(c.State), c.RestartCount, last)
}

func stateSummary(s ContainerState) string {
	parts := []string{s.Kind}
	if s.Reason != "" {
		parts = append(parts, "reason="+s.Reason)
	}
	if s.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exitCode=%d", *s.ExitCode))
	}
	if s.Message != "" {
		parts = append(parts, "message="+s.Message)
	}
	return strings.Join(parts, " ")
}

func resourceText(m map[string]string) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}

func eventText(e EventRecord) string {
	return fmt.Sprintf("event %s %s: %s (count %d)", e.Type, e.Reason, e.Message, e.Count)
}

// stateTime returns the most relevant timestamp of a state: finish, then start.
func stateTime(s ContainerState) *time.Time {
	if s.FinishedAt != nil {
		t := *s.FinishedAt
		return &t
	}
	if s.StartedAt != nil {
		t := *s.StartedAt
		return &t
	}
	return nil
}

// eventTime returns the last occurrence of an event, falling back to the first.
func eventTime(e EventRecord) *time.Time {
	if e.LastTimestamp != nil {
		t := *e.LastTimestamp
		return &t
	}
	if e.FirstTimestamp != nil {
		t := *e.FirstTimestamp
		return &t
	}
	return nil
}

// ImportantLine reports whether a log line looks like an error: the rule that
// keeps a log line as evidence even when it is not among the last lines.
func ImportantLine(line string) bool { return importantLine.MatchString(line) }
