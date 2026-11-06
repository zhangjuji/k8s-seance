// Package spirit holds the persona prompts for the séance. The prompt templates
// and voice cheat-sheets live in prompts/ and are embedded into the binary.
package spirit

import (
	"bytes"
	"embed"
	"fmt"
	"path"
	"strings"
	"text/template"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

//go:embed prompts
var promptFS embed.FS

const (
	personaFile   = "prompts/persona.md"
	voiceDir      = "prompts/voices"
	noRecordText  = "冥界没有留下记录"
	noDeathTime   = noRecordText
	noEvidenceTxt = "(none) " + noRecordText
)

// voiceFiles maps each cause to its cheat-sheet file. Any cause missing here
// falls back to the unknown voice.
var voiceFiles = map[autopsy.CauseOfDeath]string{
	autopsy.CauseOOMKilled:        "oomkilled",
	autopsy.CauseCrashLoopBackOff: "crashloopbackoff",
	autopsy.CauseImagePullBackOff: "imagepullbackoff",
	autopsy.CauseError:            "error",
	autopsy.CauseProbeFailure:     "probefailure",
	autopsy.CauseUnknown:          "unknown",
}

// PromptData is the input to SystemPrompt.
type PromptData struct {
	PodName   string
	Namespace string
	Cause     autopsy.CauseOfDeath
	DeathTime string
	Evidence  []autopsy.Evidence
	// Lang is the séance language. Only llm.LangEN changes the prompt: it adds
	// englishRule at the end. Any other value renders the Chinese prompt as is.
	Lang string
}

// englishRule is appended to the system prompt of an English séance. The
// Chinese prompt does not get it, so its bytes and the golden files stay the same.
const englishRule = `

## Language

This séance is held in English. Give the opening statement in English, and use no Chinese characters or Chinese punctuation in it. For follow-up questions, rule 6 still applies: answer in the language of the question. When the evidence has no record of something, say "The underworld kept no record of that."
`

// promptView is the value the persona template is executed with.
type promptView struct {
	PodName   string
	Namespace string
	Cause     string
	DeathTime string
	Voice     string
	Evidence  string
}

// Voice returns the voice cheat-sheet for a cause. An empty or unknown cause
// returns the unknown voice.
func Voice(cause autopsy.CauseOfDeath) string {
	name, ok := voiceFiles[cause]
	if !ok {
		name = voiceFiles[autopsy.CauseUnknown]
	}
	b, err := promptFS.ReadFile(path.Join(voiceDir, name+".md"))
	if err != nil {
		b, _ = promptFS.ReadFile(path.Join(voiceDir, voiceFiles[autopsy.CauseUnknown]+".md"))
	}
	return strings.TrimSpace(string(b))
}

// SystemPrompt renders persona.md with the voice for the cause and one line per
// evidence item, in the form "[E1] (kind) text".
func SystemPrompt(d PromptData) (string, error) {
	raw, err := promptFS.ReadFile(personaFile)
	if err != nil {
		return "", fmt.Errorf("spirit: read persona: %w", err)
	}
	tmpl, err := template.New("persona").Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("spirit: parse persona: %w", err)
	}

	cause := d.Cause
	if cause == "" {
		cause = autopsy.CauseUnknown
	}
	deathTime := d.DeathTime
	if deathTime == "" {
		deathTime = noDeathTime
	}

	view := promptView{
		PodName:   d.PodName,
		Namespace: d.Namespace,
		Cause:     string(cause),
		DeathTime: deathTime,
		Voice:     Voice(cause),
		Evidence:  formatEvidence(d.Evidence),
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, view); err != nil {
		return "", fmt.Errorf("spirit: render persona: %w", err)
	}
	if llm.NormalizeLang(d.Lang) == llm.LangEN {
		buf.WriteString(strings.TrimPrefix(englishRule, "\n"))
	}
	return buf.String(), nil
}

// oneLine turns a text into a single line so that each evidence item stays on
// its own row in the prompt.
var oneLine = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

func formatEvidence(items []autopsy.Evidence) string {
	if len(items) == 0 {
		return noEvidenceTxt
	}
	lines := make([]string, 0, len(items))
	for _, e := range items {
		lines = append(lines, fmt.Sprintf("[%s] (%s) %s", e.ID, e.Kind, strings.TrimSpace(oneLine.Replace(e.Text))))
	}
	return strings.Join(lines, "\n")
}
