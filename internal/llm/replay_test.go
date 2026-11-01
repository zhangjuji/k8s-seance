package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// fakeProvider is an Inner or Fallback stand-in. It sends reply as one fragment,
// or returns err, and counts how often it was called.
type fakeProvider struct {
	reply string
	usage Usage
	err   error

	mu    sync.Mutex
	calls int
}

func (f *fakeProvider) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Usage, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return Usage{}, f.err
	}
	if err := onDelta(f.reply); err != nil {
		return Usage{}, err
	}
	return f.usage, nil
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// streamResult is what a caller sees from one Stream call.
type streamResult struct {
	text   string
	chunks []string
	usage  Usage
	err    error
}

func streamAll(ctx context.Context, p Provider, req ChatRequest) streamResult {
	var r streamResult
	var b strings.Builder
	r.usage, r.err = p.Stream(ctx, req, func(s string) error {
		r.chunks = append(r.chunks, s)
		b.WriteString(s)
		return nil
	})
	r.text = b.String()
	return r
}

// oomRequest is a request for the fixture with the given pod name and turn.
func oomRequest(fixture string, turn int) ChatRequest {
	return ChatRequest{
		Messages: []Message{{Role: "user", Content: "opening"}},
		Cause:    autopsy.CauseOOMKilled,
		PodName:  fixture,
		Evidence: []autopsy.Evidence{
			{ID: "E1", Kind: "status", Text: "container stress lastState: terminated reason=OOMKilled exitCode=137"},
			{ID: "E2", Kind: "spec", Text: "container stress limits: memory=64Mi"},
		},
		FixtureID: fixture,
		TurnIndex: turn,
	}
}

// mustJSON encodes v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return data
}

const recordedReply = "我的内存一点点涨满，窒息的时候，我越来越重。证据在此：「reason=OOMKilled」[E1]。"

func oomRecording() recording {
	return recording{
		FixtureID: "oom-demo",
		Turns: []recordedTurn{
			{
				TurnIndex: 0,
				Cause:     "OOMKilled",
				LastUser:  "opening",
				Reply:     recordedReply,
				Usage:     recordedUsage{InputTokens: 812, OutputTokens: 96},
			},
		},
	}
}

func TestReplayHitStreamsRecordedTextAndUsage(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, oomRecording())}}
	fallback := &fakeProvider{reply: "should not be used"}
	p := &ReplayProvider{FS: fsys, Fallback: fallback}

	got := streamAll(context.Background(), p, oomRequest("oom-demo", 0))
	if got.err != nil {
		t.Fatalf("Stream: %v", got.err)
	}
	if got.text != recordedReply {
		t.Errorf("text = %q, want %q", got.text, recordedReply)
	}
	if want := (Usage{InputTokens: 812, OutputTokens: 96}); got.usage != want {
		t.Errorf("usage = %+v, want %+v", got.usage, want)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback called %d times on a hit", fallback.callCount())
	}
	if len(got.chunks) < 2 {
		t.Errorf("got %d chunks, want the reply streamed in several", len(got.chunks))
	}
	for i, c := range got.chunks {
		if n := utf8.RuneCountInString(c); n == 0 || n > chunkRunes {
			t.Errorf("chunk %d has %d runes, want 1..%d", i, n, chunkRunes)
		}
	}
}

func TestReplayMissFallsBackToMock(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, oomRecording())}}
	noRecordingFile := fstest.MapFS{}

	tests := []struct {
		name string
		fs   fs.FS
		req  ChatRequest
	}{
		{"turn index not recorded", fsys, oomRequest("oom-demo", 1)},
		{"unknown fixture", fsys, oomRequest("other-pod", 0)},
		{"empty fixture id", fsys, oomRequest("", 0)},
		{"fixture id has no usable characters", fsys, oomRequest("...", 0)},
		{"no recording file", noRecordingFile, oomRequest("oom-demo", 0)},
		{"nil FS", nil, oomRequest("oom-demo", 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := streamAll(context.Background(), MockProvider{}, tt.req)
			if want.err != nil || !strings.Contains(want.text, "[E") {
				t.Fatalf("mock baseline broken: err=%v text=%q", want.err, want.text)
			}

			got := streamAll(context.Background(), &ReplayProvider{FS: tt.fs}, tt.req)
			if got.err != nil {
				t.Fatalf("Stream: %v", got.err)
			}
			if got.text != want.text {
				t.Errorf("text = %q, want mock output %q", got.text, want.text)
			}
			if got.usage != want.usage {
				t.Errorf("usage = %+v, want mock usage %+v", got.usage, want.usage)
			}
			if !strings.Contains(got.text, "[E") {
				t.Errorf("fallback text %q has no [E#] citation", got.text)
			}
		})
	}
}

func TestReplayCustomFallbackIsUsed(t *testing.T) {
	fallback := &fakeProvider{reply: "fallback line", usage: Usage{InputTokens: 1, OutputTokens: 2}}
	p := &ReplayProvider{FS: fstest.MapFS{}, Fallback: fallback}

	got := streamAll(context.Background(), p, oomRequest("oom-demo", 0))
	if got.err != nil || got.text != "fallback line" {
		t.Fatalf("got text=%q err=%v, want fallback line", got.text, got.err)
	}
	if got.usage != fallback.usage {
		t.Errorf("usage = %+v, want %+v", got.usage, fallback.usage)
	}
	if fallback.callCount() != 1 {
		t.Errorf("fallback calls = %d, want 1", fallback.callCount())
	}
}

func TestReplayInvalidJSONFallsBackWithoutError(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: []byte("{not json")}}
	req := oomRequest("oom-demo", 0)

	want := streamAll(context.Background(), MockProvider{}, req)
	got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
	if got.err != nil {
		t.Fatalf("Stream returned error %v for invalid JSON, want mock fallback with no error", got.err)
	}
	if got.text != want.text {
		t.Errorf("text = %q, want mock output %q", got.text, want.text)
	}
}

func TestReplayFixtureIDMismatchIsMiss(t *testing.T) {
	rec := oomRecording()
	rec.FixtureID = "someone-else"
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}

	fallback := &fakeProvider{reply: "fallback"}
	got := streamAll(context.Background(), &ReplayProvider{FS: fsys, Fallback: fallback}, oomRequest("oom-demo", 0))
	if got.err != nil || got.text != "fallback" {
		t.Errorf("got text=%q err=%v, want fallback", got.text, got.err)
	}
}

func TestReplayCacheReadsFileOnce(t *testing.T) {
	// fstest.MapFS cannot count reads, so use a file that is loaded once and then
	// changed. A second read would see the new content and give a different reply.
	dir := t.TempDir()
	path := filepath.Join(dir, "oom-demo.json")
	if err := os.WriteFile(path, mustJSON(t, oomRecording()), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &ReplayProvider{FS: os.DirFS(dir)}
	if got := streamAll(context.Background(), p, oomRequest("oom-demo", 0)); got.text != recordedReply {
		t.Fatalf("first call text = %q", got.text)
	}

	changed := oomRecording()
	changed.Turns[0].Reply = "changed"
	if err := os.WriteFile(path, mustJSON(t, changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := streamAll(context.Background(), p, oomRequest("oom-demo", 0)); got.text != recordedReply {
		t.Errorf("second call text = %q, want cached %q", got.text, recordedReply)
	}
}

func TestReplayConcurrentStreams(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, oomRecording())}}
	p := &ReplayProvider{FS: fsys}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := streamAll(context.Background(), p, oomRequest("oom-demo", 0))
			if got.err != nil || got.text != recordedReply {
				errs <- fmt.Errorf("text=%q err=%v", got.text, got.err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestReplayContextCancelled(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, oomRecording())}}

	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := (&ReplayProvider{FS: fsys}).Stream(ctx, oomRequest("oom-demo", 0), func(string) error {
			t.Error("onDelta called after cancel")
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("cancelled between chunks", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &ReplayProvider{FS: fsys, Delay: 10 * time.Millisecond}
		chunks := 0
		_, err := p.Stream(ctx, oomRequest("oom-demo", 0), func(string) error {
			chunks++
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		if chunks != 1 {
			t.Errorf("got %d chunks after cancel, want 1", chunks)
		}
	})

	t.Run("cancelled during delay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		p := &ReplayProvider{FS: fsys, Delay: time.Hour}
		start := time.Now()
		_, err := p.Stream(ctx, oomRequest("oom-demo", 0), func(string) error { return nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("Stream took %v after cancel, want prompt return", elapsed)
		}
	})
}

func TestReplayOnDeltaErrorStopsStream(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, oomRecording())}}
	sentinel := errors.New("client gone")
	calls := 0
	_, err := (&ReplayProvider{FS: fsys}).Stream(context.Background(), oomRequest("oom-demo", 0), func(string) error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Errorf("onDelta called %d times, want 1", calls)
	}
}

func TestReplayNilOnDelta(t *testing.T) {
	_, err := (&ReplayProvider{}).Stream(context.Background(), oomRequest("oom-demo", 0), nil)
	if err == nil {
		t.Error("nil onDelta returned no error")
	}
}

func TestFixtureFileName(t *testing.T) {
	tests := []struct {
		id     string
		want   string
		wantOK bool
	}{
		{"memory-glutton-7cc6df9966-bf87c", "memory-glutton-7cc6df9966-bf87c.json", true},
		{"Oom Demo/1", "oom-demo-1.json", true},
		{"../etc/passwd", "---etc-passwd.json", true},
		{"under_score", "under_score.json", true},
		{"", "", false},
		{"...", "---.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got, ok := fixtureFileName(tt.id)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("fixtureFileName(%q) = %q, %v; want %q, %v", tt.id, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestRecorderWritesThenReplayReturnsSameReply(t *testing.T) {
	dir := t.TempDir()
	inner := &fakeProvider{reply: recordedReply, usage: Usage{InputTokens: 812, OutputTokens: 96}}
	rec := &Recorder{Inner: inner, Dir: dir}

	live := streamAll(context.Background(), rec, oomRequest("Oom Demo", 0))
	if live.err != nil {
		t.Fatalf("record: %v", live.err)
	}
	if live.text != recordedReply {
		t.Errorf("recorder streamed %q, want %q", live.text, recordedReply)
	}
	if _, err := os.Stat(filepath.Join(dir, "oom-demo.json")); err != nil {
		t.Fatalf("expected oom-demo.json: %v", err)
	}

	fallback := &fakeProvider{reply: "fallback"}
	replayed := streamAll(context.Background(), &ReplayProvider{FS: os.DirFS(dir), Fallback: fallback}, oomRequest("Oom Demo", 0))
	if replayed.err != nil {
		t.Fatalf("replay: %v", replayed.err)
	}
	if replayed.text != live.text {
		t.Errorf("replay text = %q, want %q", replayed.text, live.text)
	}
	if replayed.usage != live.usage {
		t.Errorf("replay usage = %+v, want %+v", replayed.usage, live.usage)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.callCount())
	}
}

func TestRecorderStoresCauseAndLastUser(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Inner: &fakeProvider{reply: "r"}, Dir: dir}
	req := oomRequest("oom-demo", 2)
	req.Messages = []Message{
		{Role: "user", Content: "opening"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "why?"},
	}
	if _, err := rec.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stored := readStoredRecording(t, dir, "oom-demo.json")
	if len(stored.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(stored.Turns))
	}
	turn := stored.Turns[0]
	if turn.TurnIndex != 2 || turn.Cause != "OOMKilled" || turn.LastUser != "why?" {
		t.Errorf("turn = %+v, want index 2, cause OOMKilled, lastUser %q", turn, "why?")
	}
}

func TestRecorderUpsertReplacesSameTurn(t *testing.T) {
	// A recorded turn is answered from disk, so replacement only happens on a miss:
	// a different question at an index that already has a turn replaces that turn.
	dir := t.TempDir()
	first := &Recorder{Inner: &fakeProvider{reply: "first", usage: Usage{InputTokens: 1, OutputTokens: 1}}, Dir: dir}
	second := &Recorder{Inner: &fakeProvider{reply: "second", usage: Usage{InputTokens: 2, OutputTokens: 2}}, Dir: dir}
	third := &Recorder{Inner: &fakeProvider{reply: "third"}, Dir: dir}
	nop := func(string) error { return nil }

	if _, err := first.Stream(context.Background(), chatRequest(1, "怎么修复？"), nop); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Stream(context.Background(), chatRequest(1, "谁删了你？"), nop); err != nil {
		t.Fatal(err)
	}
	if _, err := third.Stream(context.Background(), chatRequest(2, "别的问题"), nop); err != nil {
		t.Fatal(err)
	}

	stored := readStoredRecording(t, dir, "oom-demo.json")
	if len(stored.Turns) != 2 {
		t.Fatalf("turns = %+v, want 2 (replaced turn 1, added turn 2)", stored.Turns)
	}
	if stored.Turns[0].TurnIndex != 1 || stored.Turns[0].Reply != "second" {
		t.Errorf("turn 1 = %+v, want reply %q", stored.Turns[0], "second")
	}
	if want := (recordedUsage{InputTokens: 2, OutputTokens: 2}); stored.Turns[0].Usage != want {
		t.Errorf("turn 1 usage = %+v, want %+v", stored.Turns[0].Usage, want)
	}
	if stored.Turns[1].TurnIndex != 2 || stored.Turns[1].Reply != "third" {
		t.Errorf("turn 2 = %+v, want reply %q", stored.Turns[1], "third")
	}
}

func TestRecorderConcurrentWritesKeepAllTurns(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Inner: &fakeProvider{reply: "r"}, Dir: dir}
	const n = 8

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(turn int) {
			defer wg.Done()
			if _, err := rec.Stream(context.Background(), chatRequest(turn, fmt.Sprintf("question %d", turn)), func(string) error { return nil }); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if stored := readStoredRecording(t, dir, "oom-demo.json"); len(stored.Turns) != n {
		t.Errorf("turns = %d, want %d", len(stored.Turns), n)
	}
}

func TestRecorderSkipsFailedCalls(t *testing.T) {
	dir := t.TempDir()
	rec := &Recorder{Inner: &fakeProvider{err: errors.New("upstream 500")}, Dir: dir}
	_, err := rec.Stream(context.Background(), oomRequest("oom-demo", 0), func(string) error { return nil })
	if err == nil {
		t.Fatal("expected the inner error")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "oom-demo.json")); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("file exists after failed call: %v", statErr)
	}
}

func TestRecorderRefusesToOverwriteBadFile(t *testing.T) {
	t.Run("corrupt JSON", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "oom-demo.json")
		if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := &Recorder{Inner: &fakeProvider{reply: "r"}, Dir: dir}
		if _, err := rec.Stream(context.Background(), oomRequest("oom-demo", 0), func(string) error { return nil }); err == nil {
			t.Fatal("expected decode error")
		}
		if data, _ := os.ReadFile(path); string(data) != "{broken" {
			t.Errorf("corrupt file was overwritten: %q", data)
		}
	})

	t.Run("another fixture's file", func(t *testing.T) {
		dir := t.TempDir()
		// "OOM" and "oom" both sanitize to oom.json, so the second ID must not overwrite the first.
		if err := os.WriteFile(filepath.Join(dir, "oom.json"), mustJSON(t, oomRecording()), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := &Recorder{Inner: &fakeProvider{reply: "r"}, Dir: dir}
		if _, err := rec.Stream(context.Background(), oomRequest("OOM", 0), func(string) error { return nil }); err == nil {
			t.Fatal("expected fixture mismatch error")
		}
		if stored := readStoredRecording(t, dir, "oom.json"); stored.FixtureID != "oom-demo" {
			t.Errorf("fixtureId changed to %q", stored.FixtureID)
		}
	})
}

func TestRecorderRequiresDirAndID(t *testing.T) {
	nop := func(string) error { return nil }
	if _, err := (&Recorder{Inner: &fakeProvider{reply: "r"}}).Stream(context.Background(), oomRequest("oom-demo", 0), nop); err == nil {
		t.Error("empty Dir: want error")
	}
	if _, err := (&Recorder{Inner: &fakeProvider{reply: "r"}, Dir: t.TempDir()}).Stream(context.Background(), oomRequest("", 0), nop); err == nil {
		t.Error("empty fixture ID: want error")
	}
	if _, err := (&Recorder{Dir: t.TempDir()}).Stream(context.Background(), oomRequest("oom-demo", 0), nop); err == nil {
		t.Error("nil Inner: want error")
	}
}

// readStoredRecording decodes a recording file written by the Recorder.
func readStoredRecording(t *testing.T, dir, name string) recording {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var rec recording
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return rec
}

func TestNewProviderModeMatrix(t *testing.T) {
	complete := Config{BaseURL: "https://example.invalid/v1", APIKey: "k", Model: "m"}
	incomplete := Config{}
	replayFS := fstest.MapFS{}

	tests := []struct {
		name     string
		cfg      Config
		mode     string
		env      string
		record   bool
		wantType string // replay, mock, http, recorder
	}{
		{"replay mode, no record", incomplete, "replay", "", false, "replay"},
		{"replay mode ignores record", complete, "replay", "", true, "replay"},
		{"mock with record does not wrap", complete, "mock", "", true, "mock"},
		{"empty mode, complete, record wraps http", complete, "", "", true, "recorder"},
		{"empty mode, complete, no record is http", complete, "", "", false, "http"},
		{"empty mode, incomplete, record stays mock", incomplete, "", "", true, "mock"},
		{"http, complete, record wraps", complete, "http", "", true, "recorder"},
		{"http, complete, no record", complete, "http", "", false, "http"},
		{"http, incomplete, record stays mock", incomplete, "http", "", true, "mock"},
		{"env replay with empty flag", incomplete, "", "replay", false, "replay"},
		{"flag beats env: mock over env replay", complete, "mock", "replay", false, "mock"},
		{"unknown mode is mock", complete, "bogus", "", true, "mock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SEANCE_LLM_MODE", tt.env)
			got := NewProvider(tt.cfg, tt.mode, tt.record, replayFS, t.TempDir())

			var gotType string
			switch p := got.(type) {
			case *ReplayProvider:
				gotType = "replay"
				if p.FS == nil {
					t.Error("ReplayProvider has no FS")
				}
			case MockProvider:
				gotType = "mock"
			case HTTPProvider:
				gotType = "http"
			case *Recorder:
				gotType = "recorder"
				if _, ok := p.Inner.(HTTPProvider); !ok {
					t.Errorf("Recorder.Inner = %T, want HTTPProvider", p.Inner)
				}
			default:
				t.Fatalf("unexpected provider type %T", got)
			}
			if gotType != tt.wantType {
				t.Errorf("NewProvider(%q, record=%v) = %s, want %s", tt.mode, tt.record, gotType, tt.wantType)
			}
		})
	}
}
