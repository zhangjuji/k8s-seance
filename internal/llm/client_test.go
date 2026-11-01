package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// noBackoff removes the retry pause for the duration of one test.
func noBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoff
	retryBackoff = 0
	t.Cleanup(func() { retryBackoff = old })
}

// sseBody writes the given data payloads as an SSE stream.
func sseBody(w io.Writer, payloads ...string) {
	for _, p := range payloads {
		fmt.Fprintf(w, "data: %s\n\n", p)
	}
}

func deltaChunk(text string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, text)
}

func newTestProvider(t *testing.T, h http.HandlerFunc) (HTTPProvider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return HTTPProvider{
		Config: Config{BaseURL: srv.URL + "/v1", APIKey: "sk-test-secret", Model: "test-model"},
		Client: srv.Client(),
	}, srv
}

func testRequest() ChatRequest {
	return ChatRequest{
		Messages:  []Message{{Role: "user", Content: "how did you die?"}},
		Evidence:  []autopsy.Evidence{{ID: "E1", Kind: "status", Text: "container app state: terminated reason=OOMKilled"}},
		MaxTokens: 100,
	}
}

func TestHTTPProviderStreamsDeltas(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": keep-alive comment\n\n")
		fmt.Fprint(w, "event: message\n")
		sseBody(w,
			deltaChunk("我的"),
			deltaChunk(""),
			deltaChunk("内存涨满了。"),
			`{"choices":[{"delta":{}}]}`,
			`{"choices":[]}`,
			deltaChunk("[E1]"),
		)
		sseBody(w, "[DONE]")
	})

	var got []string
	usage, err := p.Stream(context.Background(), testRequest(), func(s string) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	if joined := strings.Join(got, ""); joined != "我的内存涨满了。[E1]" {
		t.Errorf("concatenated deltas = %q, want %q", joined, "我的内存涨满了。[E1]")
	}
	if usage.InputTokens == 0 || usage.OutputTokens == 0 {
		t.Errorf("usage = %+v, want estimated non-zero counts when server sends none", usage)
	}
}

func TestHTTPProviderUsesServerUsage(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		sseBody(w, deltaChunk("hi"),
			`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":7}}`,
			"[DONE]")
	})
	usage, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil })
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	want := Usage{InputTokens: 123, OutputTokens: 7}
	if usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
	}
}

func TestHTTPProviderRequestShape(t *testing.T) {
	var gotAuth, gotPath, gotCT string
	var gotBody map[string]any
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		if err := decodeJSON(r, &gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		sseBody(w, "[DONE]")
	})

	if _, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	if gotAuth != "Bearer sk-test-secret" {
		t.Errorf("Authorization = %q, want Bearer header with key", gotAuth)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["model"] != "test-model" || gotBody["stream"] != true || gotBody["max_tokens"] != float64(100) {
		t.Errorf("body = %v, want model, stream=true and max_tokens=100", gotBody)
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want system + user", msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || !strings.Contains(sys["content"].(string), "E1 [status] container app state") {
		t.Errorf("system message = %v, want evidence line E1", sys)
	}
	if user := msgs[1].(map[string]any); user["role"] != "user" || user["content"] != "how did you die?" {
		t.Errorf("user message = %v", user)
	}
}

func TestHTTPProviderDefaultMaxTokens(t *testing.T) {
	var gotMax float64
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = decodeJSON(r, &body)
		gotMax, _ = body["max_tokens"].(float64)
		sseBody(w, "[DONE]")
	})
	req := testRequest()
	req.MaxTokens = 0
	if _, err := p.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	if gotMax != float64(defaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", gotMax, defaultMaxTokens)
	}
}

func TestHTTPProviderDoneStopsReading(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		sseBody(w, deltaChunk("a"), "[DONE]", deltaChunk("ignored"))
	})
	var got []string
	_, err := p.Stream(context.Background(), testRequest(), func(s string) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	if strings.Join(got, "") != "a" {
		t.Errorf("deltas = %q, want only %q before [DONE]", got, "a")
	}
}

func TestHTTPProviderRetriesOnce5xx(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "upstream down", http.StatusInternalServerError)
			return
		}
		sseBody(w, deltaChunk("ok"), "[DONE]")
	})
	var got string
	_, err := p.Stream(context.Background(), testRequest(), func(s string) error {
		got += s
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: unexpected error after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2", calls.Load())
	}
	if got != "ok" {
		t.Errorf("deltas = %q, want %q", got, "ok")
	}
}

func TestHTTPProviderRetriesOn429(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		sseBody(w, "[DONE]")
	})
	if _, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil }); err != nil {
		t.Fatalf("Stream: unexpected error: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("server calls = %d, want 2", calls.Load())
	}
}

func TestHTTPProviderGivesUpAfterSecondFailure(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still down", http.StatusBadGateway)
	})
	_, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil })
	if err == nil {
		t.Fatal("Stream: want error after two 502 responses, got nil")
	}
	if calls.Load() != maxAttempts {
		t.Errorf("server calls = %d, want %d", calls.Load(), maxAttempts)
	}
}

func TestHTTPProvider401NoRetry(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"invalid key"}`, http.StatusUnauthorized)
	})
	_, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil })
	if err == nil {
		t.Fatal("Stream: want error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want it to mention status 401", err)
	}
	if strings.Contains(err.Error(), "sk-test-secret") {
		t.Errorf("error leaks the API key: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on 401)", calls.Load())
	}
}

func TestHTTPProviderNoRetryAfterDelta(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sseBody(w, deltaChunk("partial"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drop the connection mid-stream
	})
	var got string
	_, err := p.Stream(context.Background(), testRequest(), func(s string) error {
		got += s
		return nil
	})
	if err == nil {
		t.Fatal("Stream: want error for a dropped stream, got nil")
	}
	if got != "partial" || calls.Load() != 1 {
		t.Errorf("got %q after %d calls, want %q after 1 call (no retry once a delta was sent)", got, calls.Load(), "partial")
	}
}

func TestHTTPProviderOnDeltaErrorStops(t *testing.T) {
	noBackoff(t)
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sseBody(w, deltaChunk("a"), deltaChunk("b"), "[DONE]")
	})
	sentinel := errors.New("client went away")
	_, err := p.Stream(context.Background(), testRequest(), func(string) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("Stream error = %v, want the onDelta error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server calls = %d, want 1", calls.Load())
	}
}

func TestHTTPProviderStreamErrorChunk(t *testing.T) {
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		sseBody(w, `{"error":{"message":"context length exceeded"}}`)
	})
	_, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "context length exceeded") {
		t.Fatalf("Stream error = %v, want the stream error message", err)
	}
}

func TestHTTPProviderContextCancel(t *testing.T) {
	noBackoff(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := p.Stream(ctx, testRequest(), func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("cancel took %v, want prompt return", elapsed)
	}
}

func TestHTTPProviderCancelledBeforeCall(t *testing.T) {
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		sseBody(w, "[DONE]")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Stream(ctx, testRequest(), func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream error = %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Errorf("server calls = %d, want 0", calls.Load())
	}
}

func TestHTTPProviderNotConfigured(t *testing.T) {
	var calls atomic.Int32
	p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	})
	p.APIKey = ""
	if _, err := p.Stream(context.Background(), testRequest(), func(string) error { return nil }); err == nil {
		t.Fatal("Stream: want error for incomplete config, got nil")
	}
	if calls.Load() != 0 {
		t.Errorf("server calls = %d, want 0", calls.Load())
	}
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func TestHTTPProviderSendsOneSystemMessage(t *testing.T) {
	const persona = "PERSONA: you are the ghost. [E1] already in here."
	tests := []struct {
		name     string
		messages []Message
		// wantSys is the content of the one system message expected in the body.
		wantSys  string
		wantRole []string
	}{
		{
			name:     "no system message in request gets placeholder with evidence",
			messages: []Message{{Role: "user", Content: "how did you die?"}},
			wantSys:  "", // checked with Contains below
			wantRole: []string{"system", "user"},
		},
		{
			name:     "persona system message is sent without placeholder",
			messages: []Message{{Role: "system", Content: persona}, {Role: "user", Content: "how did you die?"}},
			wantSys:  persona,
			wantRole: []string{"system", "user"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody map[string]any
			p, _ := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				if err := decodeJSON(r, &gotBody); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				sseBody(w, "[DONE]")
			})
			req := testRequest()
			req.Messages = tt.messages
			if _, err := p.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
				t.Fatalf("Stream: unexpected error: %v", err)
			}

			msgs, _ := gotBody["messages"].([]any)
			var roles []string
			var systems []string
			for _, m := range msgs {
				mm := m.(map[string]any)
				role := mm["role"].(string)
				roles = append(roles, role)
				if role == "system" {
					systems = append(systems, mm["content"].(string))
				}
			}
			if strings.Join(roles, ",") != strings.Join(tt.wantRole, ",") {
				t.Fatalf("roles = %v, want %v", roles, tt.wantRole)
			}
			if len(systems) != 1 {
				t.Fatalf("system messages = %d, want exactly 1", len(systems))
			}
			if tt.wantSys != "" {
				if systems[0] != tt.wantSys {
					t.Errorf("system content = %q, want %q", systems[0], tt.wantSys)
				}
			} else if !strings.Contains(systems[0], "E1 [status] container app state") {
				t.Errorf("placeholder system = %q, want evidence line E1", systems[0])
			}
		})
	}
}
