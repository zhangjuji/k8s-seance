package spirit

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// Finding kinds. They are the values of Finding.Kind.
const (
	FindingCitation = "citation" // an [E#] that names no evidence item
	FindingMemory   = "memory"   // a memory quantity that no evidence item contains
	FindingExitCode = "exitCode" // an exit code that no evidence item contains
	FindingRestarts = "restarts" // a restart count that no evidence item contains
	FindingTime     = "time"     // a timestamp that matches no evidence time and not the death time
)

// Finding is one hard fact in a reply that the evidence does not back up. Claim
// is the fact as the reply wrote it, and Sentence is the sentence it came from.
type Finding struct {
	Kind     string `json:"kind"`
	Claim    string `json:"claim"`
	Sentence string `json:"sentence"`
}

// FactCheck compares the hard facts of a spirit reply with the evidence. It is
// a pure function and makes no LLM call. known holds extra trusted text, such
// as the formatted time of death. These facts are checked:
//
//   - every [E#] must name an evidence item;
//   - memory quantities such as 256Mi must appear in some evidence text, with the
//     unit as written (64M and 64Mi differ);
//   - exit codes must appear as exitCode=N in the evidence;
//   - restart counts must appear as "restarts: N" or "(count N)" in the evidence;
//   - timestamps must match an evidence timestamp, a timestamp in the evidence
//     text or a known text, to the minute.
//
// Sentences that give advice (建议, consider, ...) may propose new memory
// quantities, so memory is not checked there. Citations, exit codes, restart
// counts and times are checked in every sentence. Text quoted from the evidence
// is not checked for quantities.
//
// FactCheck reports only contradictions. Names, paths and the story are not
// judged, so no findings means that no contradiction was found, not that the
// reply is verified. Each claim is reported once, in reply order.
func FactCheck(reply string, evidence []autopsy.Evidence, known ...string) []Finding {
	facts := collectFacts(evidence, known)
	c := &checker{facts: facts, seen: make(map[string]bool)}
	for _, sentence := range splitSentences(reply) {
		c.citations(sentence)
		body := facts.stripQuotes(sentence)
		if !isAdviceSentence(sentence) {
			c.memory(sentence, body)
		}
		c.exitCodes(sentence, body)
		c.restarts(sentence, body)
		c.times(sentence, body)
	}
	return c.out
}

// FactCheck checks a reply of this séance against its evidence and its time of
// death. See the package function FactCheck.
func (s *Session) FactCheck(reply string) []Finding {
	if s.deathTime == "" {
		return FactCheck(reply, s.Evidence)
	}
	return FactCheck(reply, s.Evidence, s.deathTime)
}

var (
	// citationRef matches [E3] and lists such as [E3, E4].
	citationRef = regexp.MustCompile(`\[\s*(E\d+(?:\s*[,，、]\s*E\d+)*)\s*\]`)
	citationID  = regexp.MustCompile(`E\d+`)

	// memoryQty matches a memory quantity with a binary or byte unit, with an
	// optional space, or a bare decimal suffix (K, M, G, T, P, E) with no space.
	// Lowercase suffixes are left out, so CPU (100m) and durations (5m0s) do not match.
	memoryQty = regexp.MustCompile(`(?:^|[^\w.])(\d+(?:\.\d+)?(?:\s?(?:[KMGTPE]i(?:B)?|[KMGTPE]B)|[KMGTPE]))\b`)

	// exitCodeClaim matches exitCode=137, exit code 1, Exit code: 1, exited with code 2 and 退出码 1.
	exitCodeClaim = regexp.MustCompile(`(?i)(?:\bexit[\s_-]?code|\bexited\s+with\s+(?:exit\s+)?(?:code|status)|退出(?:状态)?码)\s*(?:[=:：]|\bis\b|\bwas\b|\bof\b|为|是)?\s*(-?\d+)`)
	// exitCodeFact matches exitCode=N in the evidence.
	exitCodeFact = regexp.MustCompile(`exitCode=(-?\d+)`)

	// restartClaims match the restart counts written in a reply. Group 1 is the number.
	restartClaims = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brestart(?:s|Count|\s+count)\s*(?:[=:：]|\bis\b|\bwas\b|\bof\b)\s*(` + numWord + `)`),
		regexp.MustCompile(`(?i)\brestarted\s+(?:a\s+total\s+of\s+)?(` + numWord + `)\s+times?\b`),
		regexp.MustCompile(`(?i)\b((?:not\s+)?` + numWord + `)\s+restarts?\b`),
		regexp.MustCompile(`重启(?:了|过)?\s*(` + zhNum + `)\s*次`),
		regexp.MustCompile(`重启次数\s*(?:[=:：]|为|是|达到?)?\s*(` + zhNum + `)`),
		regexp.MustCompile(`(` + zhNum + `)\s*次重启`),
	}
	// restartFacts match the counts in the evidence: "restarts: N" and "(count N)".
	restartFacts = regexp.MustCompile(`restarts: (\d+)|\(count (\d+)\)`)

	// timestampRe matches 2026-11-01 04:55:23 UTC, 2026-11-01T04:55Z and similar.
	// Groups: date, hour, minute, zone.
	timestampRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})[ T](\d{2}):(\d{2})(?::\d{2}(?:\.\d+)?)?(?:\s*(Z|UTC|[+-]\d{2}:?\d{2})\b|Z)?`)

	// quoted matches a quoted span: 「…」, “…”, "…" or `…`.
	quoted = regexp.MustCompile("「([^」]*)」|“([^”]*)”|\"([^\"\\n]*)\"|`([^`\\n]*)`")

	// adviceSentence marks a sentence that suggests a change.
	// Words that also tell a story ("could not", "增加了") are left out, so a
	// narrative sentence keeps its memory check.
	adviceSentence = regexp.MustCompile(`(?i)建议|考虑|推荐|不妨|应该|应当|可以把|可以将|改成|改为|调到|调大|调高|上调|设为|设置为|设成|\b(?:consider|suggest|suggested|recommend|recommended|should|try|raise|raising|increase|bump)\b`)
)

// numWord is a number as digits or English words, such as 8, eight or twenty-one.
// The words are parsed by parseNumber, which rejects words that are not numbers.
const numWord = `(?:\d+|no|[a-z]+(?:-[a-z]+)?)`

// zhNum is a number as digits or Chinese numerals up to 九十九.
const zhNum = `(?:\d+|[零〇一二两三四五六七八九十]+)`

// facts holds what the evidence and the known texts say.
type facts struct {
	ids      map[string]bool
	memory   map[string]bool
	exit     map[int]bool
	restarts map[int]bool
	minutes  map[string]bool
	texts    []string
}

func collectFacts(evidence []autopsy.Evidence, known []string) *facts {
	f := &facts{
		ids:      make(map[string]bool),
		memory:   make(map[string]bool),
		exit:     make(map[int]bool),
		restarts: make(map[int]bool),
		minutes:  make(map[string]bool),
	}
	for _, e := range evidence {
		f.ids[e.ID] = true
		if e.Timestamp != nil {
			f.minutes[minuteKey(*e.Timestamp)] = true
		}
		f.addText(e.Text)
		for _, m := range exitCodeFact.FindAllStringSubmatch(e.Text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				f.exit[n] = true
			}
		}
		for _, m := range restartFacts.FindAllStringSubmatch(e.Text, -1) {
			if n, err := strconv.Atoi(m[1] + m[2]); err == nil {
				f.restarts[n] = true
			}
		}
	}
	for _, k := range known {
		f.addText(k)
	}
	return f
}

// addText records the memory quantities and timestamps of one trusted text.
func (f *facts) addText(text string) {
	f.texts = append(f.texts, text)
	for _, q := range findMemory(text) {
		f.memory[q.norm] = true
	}
	for _, ts := range findTimes(text) {
		f.minutes[ts.key] = true
	}
}

// stripQuotes blanks every quoted span that is a verbatim piece of trusted text,
// so a quoted (and possibly cut off) evidence line is not checked again.
func (f *facts) stripQuotes(sentence string) string {
	return quoted.ReplaceAllStringFunc(sentence, func(span string) string {
		m := quoted.FindStringSubmatch(span)
		inner := strings.TrimSpace(m[1] + m[2] + m[3] + m[4])
		inner = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(inner, "…"), "..."))
		if inner == "" {
			return span
		}
		for _, t := range f.texts {
			if strings.Contains(t, inner) {
				return " "
			}
		}
		return span
	})
}

// checker collects findings and drops repeated claims.
type checker struct {
	facts *facts
	seen  map[string]bool
	out   []Finding
}

func (c *checker) report(kind, claim, sentence string) {
	key := kind + "\x00" + claim
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.out = append(c.out, Finding{Kind: kind, Claim: claim, Sentence: sentence})
}

func (c *checker) citations(sentence string) {
	for _, m := range citationRef.FindAllStringSubmatch(sentence, -1) {
		for _, id := range citationID.FindAllString(m[1], -1) {
			if !c.facts.ids[id] {
				c.report(FindingCitation, "["+id+"]", sentence)
			}
		}
	}
}

func (c *checker) memory(sentence, body string) {
	for _, q := range findMemory(body) {
		if !c.facts.memory[q.norm] {
			c.report(FindingMemory, q.text, sentence)
		}
	}
}

func (c *checker) exitCodes(sentence, body string) {
	for _, m := range exitCodeClaim.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err == nil && !c.facts.exit[n] {
			c.report(FindingExitCode, strings.TrimSpace(m[0]), sentence)
		}
	}
}

func (c *checker) restarts(sentence, body string) {
	for _, re := range restartClaims {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			n, ok := parseNumber(m[1])
			if ok && !c.facts.restarts[n] {
				c.report(FindingRestarts, strings.TrimSpace(m[0]), sentence)
			}
		}
	}
}

func (c *checker) times(sentence, body string) {
	for _, ts := range findTimes(body) {
		if !c.facts.minutes[ts.key] {
			c.report(FindingTime, ts.text, sentence)
		}
	}
}

// memoryMatch is one memory quantity: as written, and normalized ("64 MiB" is "64Mi").
type memoryMatch struct{ text, norm string }

func findMemory(s string) []memoryMatch {
	var out []memoryMatch
	for _, m := range memoryQty.FindAllStringSubmatch(s, -1) {
		text := m[1]
		norm := strings.ReplaceAll(text, " ", "")
		if strings.HasSuffix(norm, "iB") {
			norm = strings.TrimSuffix(norm, "B") // 64MiB is 64Mi
		} else if strings.HasSuffix(norm, "B") {
			norm = strings.TrimSuffix(norm, "B") // 64MB is 64M
		}
		out = append(out, memoryMatch{text: text, norm: norm})
	}
	return out
}

// timeMatch is one timestamp: as written, and its UTC minute key.
type timeMatch struct{ text, key string }

// findTimes returns the timestamps in s. A timestamp without a zone is UTC,
// because the spirit is given UTC times.
func findTimes(s string) []timeMatch {
	var out []timeMatch
	for _, m := range timestampRe.FindAllStringSubmatch(s, -1) {
		layout, value := "2006-01-02 15:04", m[1]+" "+m[2]+":"+m[3]
		switch zone := m[4]; {
		case zone == "" || zone == "Z" || zone == "UTC":
		default:
			layout += " -0700"
			value += " " + strings.ReplaceAll(zone, ":", "")
		}
		t, err := time.Parse(layout, value)
		if err != nil {
			continue
		}
		out = append(out, timeMatch{text: strings.TrimSpace(m[0]), key: minuteKey(t)})
	}
	return out
}

func minuteKey(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04")
}

// splitSentences cuts text after 。！？；, after newlines and after . ! ? ; that
// are followed by a space or the end. Decimal points and versions stay whole.
func splitSentences(text string) []string {
	var out []string
	runes := []rune(text)
	start := 0
	cut := func(end int) {
		if s := strings.TrimSpace(string(runes[start:end])); s != "" {
			out = append(out, s)
		}
		start = end
	}
	for i, r := range runes {
		switch r {
		case '。', '！', '？', '；', '\n':
			cut(i + 1)
		case '.', '!', '?', ';':
			if i+1 == len(runes) || runes[i+1] == ' ' || runes[i+1] == '\t' {
				cut(i + 1)
			}
		}
	}
	cut(len(runes))
	return out
}

func isAdviceSentence(s string) bool {
	return adviceSentence.MatchString(s)
}

// englishNumbers maps number words to values. Tens and units combine as "twenty-one".
var englishNumbers = map[string]int{
	"no": 0, "zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
	"twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
	"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30,
	"forty": 40, "fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90,
}

var chineseDigits = map[rune]int{
	'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
}

// parseNumber reads digits, English number words up to ninety-nine ("not one"
// is 0) and Chinese numerals up to 九十九. ok is false for anything else.
func parseNumber(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if rest, ok := strings.CutPrefix(s, "not"); ok && rest != s {
		if n, ok := parseNumber(rest); ok && n == 1 {
			return 0, true // "not one restart"
		}
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	if n, ok := englishNumbers[s]; ok {
		return n, true
	}
	if tens, unit, ok := strings.Cut(s, "-"); ok {
		t, ok1 := englishNumbers[tens]
		u, ok2 := englishNumbers[unit]
		if ok1 && ok2 && t >= 20 && t%10 == 0 && u >= 1 && u <= 9 {
			return t + u, true
		}
		return 0, false
	}
	return parseChinese(s)
}

// parseChinese reads 八, 十, 十二, 六十六 and 九十九.
func parseChinese(s string) (int, bool) {
	runes := []rune(s)
	if len(runes) == 0 || len(runes) > 3 {
		return 0, false
	}
	ten := slices.Index(runes, '十')
	if ten < 0 {
		if len(runes) != 1 {
			return 0, false
		}
		n, ok := chineseDigits[runes[0]]
		return n, ok
	}
	n := 10
	if ten == 1 {
		d, ok := chineseDigits[runes[0]]
		if !ok || d == 0 {
			return 0, false
		}
		n = d * 10
	} else if ten != 0 {
		return 0, false
	}
	switch rest := runes[ten+1:]; len(rest) {
	case 0:
	case 1:
		d, ok := chineseDigits[rest[0]]
		if !ok || d == 0 {
			return 0, false
		}
		n += d
	default:
		return 0, false
	}
	return n, true
}
