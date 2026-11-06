// Package llm defines the streaming LLM provider contract and its implementations.
package llm

import (
	"context"
	"io/fs"
	"os"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// Message is one chat turn sent to the model.
type Message struct {
	Role    string
	Content string
}

// Purpose values for ChatRequest.Purpose.
const (
	PurposeOpening    = "opening"
	PurposeChat       = "chat"
	PurposePostmortem = "postmortem"
)

// ChatRequest is the input for one spirit turn.
type ChatRequest struct {
	Messages  []Message
	Cause     autopsy.CauseOfDeath
	PodName   string
	Evidence  []autopsy.Evidence
	MaxTokens int
	// FixtureID is the replay key. The session code sets it to the pod name.
	FixtureID string
	// TurnIndex is 0 for the opening and 1.. for follow-ups. The postmortem
	// generator uses its own index set by its caller.
	TurnIndex int
	// Purpose is PurposeOpening, PurposeChat or PurposePostmortem. Empty means
	// an older caller that did not set it.
	Purpose string
	// DeathTime is the formatted time of death, or empty when it is unknown.
	DeathTime string
	// Lang is the séance language, LangZH or LangEN. Empty means LangZH. The
	// mock opening and the budget-exhausted line follow it. Follow-up mock
	// answers still follow the language of the question.
	Lang string
}

// Séance languages.
const (
	LangZH = "zh"
	LangEN = "en"
)

// NormalizeLang maps a language tag to LangZH or LangEN. It accepts the bare
// codes and region forms in any case, such as "en", "EN", "en-US", "zh_CN" and
// "zh-Hans". Anything else, including "", returns "".
func NormalizeLang(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	primary, _, _ := strings.Cut(strings.ReplaceAll(tag, "_", "-"), "-")
	switch primary {
	case LangZH:
		return LangZH
	case LangEN:
		return LangEN
	}
	return ""
}

// langOrDefault returns NormalizeLang(tag), or LangZH when the tag is not recognized.
func langOrDefault(tag string) string {
	if l := NormalizeLang(tag); l != "" {
		return l
	}
	return LangZH
}

// Usage holds the token counts of one call. The mock provider reports rough rune counts.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Provider streams one spirit reply. onDelta receives the text fragments in order.
// If onDelta returns an error, Stream stops and returns that error.
type Provider interface {
	Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error)
}

// Mode names accepted by SelectProvider and SEANCE_LLM_MODE.
const (
	ModeHTTP   = "http"
	ModeReplay = "replay"
	ModeMock   = "mock"
)

// resolveMode normalizes mode. If mode is empty, SEANCE_LLM_MODE is used.
func resolveMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("SEANCE_LLM_MODE")))
	}
	return mode
}

// SelectProvider picks the Provider to use. mode comes from a flag. If it is
// empty, SEANCE_LLM_MODE is used. If both are empty, the result is http when
// cfg is complete and mock otherwise. Unknown modes and incomplete config
// fall back to mock, so no call is made without a configured endpoint.
// SelectProvider has no replay files, so "replay" also gives MockProvider.
// NewProvider builds the replay provider.
func SelectProvider(cfg Config, mode string) Provider {
	switch resolveMode(mode) {
	case ModeHTTP:
		if cfg.Complete() {
			return HTTPProvider{Config: cfg}
		}
		return MockProvider{}
	case ModeReplay:
		return MockProvider{}
	case ModeMock:
		return MockProvider{}
	case "":
		if cfg.Complete() {
			return HTTPProvider{Config: cfg}
		}
		return MockProvider{}
	default:
		return MockProvider{}
	}
}

// NewProvider builds the Provider for a run. mode is resolved as in SelectProvider.
// Mode "replay" gives a ReplayProvider over replayFS, with MockProvider as the
// fallback. Otherwise the result is SelectProvider's choice. If record is true and
// that choice is an HTTPProvider, it is wrapped in a Recorder that writes into
// recordDir. A MockProvider is never wrapped, because recording mock output would
// only save the template text. A Recorder answers from recordDir when the turn is
// already recorded, so it calls the API only for missing turns.
func NewProvider(cfg Config, mode string, record bool, replayFS fs.FS, recordDir string) Provider {
	mode = resolveMode(mode)
	if mode == ModeReplay {
		return &ReplayProvider{FS: replayFS}
	}
	p := SelectProvider(cfg, mode)
	if _, ok := p.(HTTPProvider); ok && record {
		return &Recorder{Inner: p, Dir: recordDir}
	}
	return p
}
