// Package llm defines the streaming LLM provider contract and its implementations.
package llm

import (
	"context"
	"os"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// Message is one chat turn sent to the model.
type Message struct {
	Role    string
	Content string
}

// ChatRequest is the input for one spirit turn.
type ChatRequest struct {
	Messages  []Message
	Cause     autopsy.CauseOfDeath
	PodName   string
	Evidence  []autopsy.Evidence
	MaxTokens int
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

// SelectProvider picks the Provider to use. mode comes from a flag. If it is
// empty, SEANCE_LLM_MODE is used. If both are empty, the result is http when
// cfg is complete and mock otherwise. Unknown modes and incomplete config
// fall back to mock, so no call is made without a configured endpoint.
func SelectProvider(cfg Config, mode string) Provider {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv("SEANCE_LLM_MODE")))
	}
	switch mode {
	case ModeHTTP:
		if cfg.Complete() {
			return HTTPProvider{Config: cfg}
		}
		return MockProvider{}
	case ModeReplay:
		// TODO(4.4): return ReplayProvider once replay.go exists. Mock until then.
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
