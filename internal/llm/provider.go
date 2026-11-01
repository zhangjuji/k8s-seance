// Package llm defines the streaming LLM provider contract and its implementations.
package llm

import (
	"context"

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
