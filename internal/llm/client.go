package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// requestTimeout bounds one whole call, including the stream.
	requestTimeout = 15 * time.Second
	// maxAttempts is one try plus one retry.
	maxAttempts = 2
	// defaultMaxTokens is used when the request leaves MaxTokens at zero.
	defaultMaxTokens = 400
	// maxErrorBody caps how much of an error response is kept in the error text.
	maxErrorBody = 512
	// maxLineBytes caps one SSE line.
	maxLineBytes = 1 << 20
	// placeholderSystem stands in for the persona prompt until task 5.1.
	placeholderSystem = "You are the ghost of a dead Kubernetes Pod. Answer in first person, briefly, and cite evidence as [E#]."
)

// retryBackoff is the pause before the retry. Tests set it to zero.
var retryBackoff = 500 * time.Millisecond

// HTTPProvider streams from an OpenAI-compatible Chat Completions endpoint.
// A nil Client uses a default http.Client. The API key is only sent in the
// Authorization header and never written to logs or errors.
type HTTPProvider struct {
	Config
	Client *http.Client
}

var _ Provider = HTTPProvider{}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequestBody struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
}

// streamChunk is one "data:" payload of the SSE stream.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// statusError is a non-2xx response.
type statusError struct {
	Code int
	Body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("llm: HTTP %d: %s", e.Code, e.Body)
}

// transportError is a failure to get a response at all (connect, reset, timeout).
type transportError struct{ err error }

func (e *transportError) Error() string { return "llm: request failed: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// Stream implements Provider. It retries once, and only when no delta has been
// sent yet and the failure is a transport error, HTTP 429 or HTTP 5xx.
func (p HTTPProvider) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	if onDelta == nil {
		return Usage{}, errors.New("llm: nil onDelta")
	}
	if !p.Complete() {
		return Usage{}, errors.New("llm: not configured: base URL, API key and model are required")
	}
	body, err := json.Marshal(buildRequestBody(p.Model, req))
	if err != nil {
		return Usage{}, fmt.Errorf("llm: encode request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	client := p.Client
	if client == nil {
		client = &http.Client{}
	}

	for attempt := 1; ; attempt++ {
		emitted := false
		usage, err := p.attempt(ctx, client, body, req, func(s string) error {
			emitted = true
			return onDelta(s)
		})
		if err == nil {
			return usage, nil
		}
		if emitted || attempt >= maxAttempts || ctx.Err() != nil || !retryable(err) {
			return Usage{}, err
		}
		if err := sleepCtx(ctx, retryBackoff); err != nil {
			return Usage{}, err
		}
	}
}

// attempt performs one POST and streams its deltas.
func (p HTTPProvider) attempt(ctx context.Context, client *http.Client, body []byte, req ChatRequest, onDelta func(string) error) (Usage, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Usage{}, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Usage{}, ctx.Err()
		}
		return Usage{}, &transportError{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return Usage{}, &statusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(snippet))}
	}
	return readStream(resp.Body, req, onDelta)
}

// readStream parses the SSE body. It returns when it sees "data: [DONE]" or EOF.
// If the server sends no usage numbers, the counts are estimated from rune counts.
func readStream(r io.Reader, req ChatRequest, onDelta func(string) error) (Usage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var reply strings.Builder
	var serverUsage *Usage
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue // blank separators, comments, event: lines
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return Usage{}, fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return Usage{}, fmt.Errorf("llm: stream error: %s", chunk.Error.Message)
		}
		if chunk.Usage != nil {
			serverUsage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		text := chunk.Choices[0].Delta.Content
		if text == "" {
			continue
		}
		reply.WriteString(text)
		if err := onDelta(text); err != nil {
			return Usage{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return Usage{}, fmt.Errorf("llm: read stream: %w", err)
	}

	if serverUsage != nil {
		return *serverUsage, nil
	}
	return Usage{
		InputTokens:  inputRunes(req),
		OutputTokens: utf8.RuneCountInString(reply.String()),
	}, nil
}

// buildRequestBody puts the evidence into one system message, then the chat turns.
func buildRequestBody(model string, req ChatRequest) chatRequestBody {
	var sys strings.Builder
	sys.WriteString(placeholderSystem)
	if len(req.Evidence) > 0 {
		sys.WriteString("\n\nEvidence:\n")
		for _, e := range req.Evidence {
			fmt.Fprintf(&sys, "%s [%s] %s\n", e.ID, e.Kind, e.Text)
		}
	}

	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	msgs = append(msgs, chatMessage{Role: "system", Content: strings.TrimRight(sys.String(), "\n")})
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: m.Content})
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return chatRequestBody{Model: model, Messages: msgs, Stream: true, MaxTokens: maxTokens}
}

// retryable reports whether a failed attempt may be retried.
func retryable(err error) bool {
	var te *transportError
	if errors.As(err, &te) {
		return true
	}
	var se *statusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return false
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
