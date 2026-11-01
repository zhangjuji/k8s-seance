package llm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// recordDirWith writes rec as oom-demo.json into a new temp directory and returns the directory.
func recordDirWith(t *testing.T, rec recording) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oom-demo.json"), mustJSON(t, rec), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// loadStoredRecording decodes the oom-demo.json file in dir.
func loadStoredRecording(t *testing.T, dir string) recording {
	t.Helper()
	var rec recording
	if err := json.Unmarshal(readStoredBytes(t, dir, "oom-demo.json"), &rec); err != nil {
		t.Fatalf("decode stored recording: %v", err)
	}
	return rec
}

func noDelta(string) error { return nil }

func TestRecorderHitStreamsRecordedTurnWithoutCall(t *testing.T) {
	dir := recordDirWith(t, matchRecording())
	before := readStoredBytes(t, dir, "oom-demo.json")

	tests := []struct {
		name string
		req  ChatRequest
		want string
	}{
		{"opening by index", oomRequest("oom-demo", 0), "OPENING-REPLY [E1]"},
		{"follow-up by question", chatRequest(1, "怎么修复？"), fixMsg},
		{"follow-up question variant", chatRequest(7, "怎么修复"), fixMsg},
		{"postmortem by index", chatRequest(PostmortemTurnIndex, "any transcript"), "POSTMORTEM-REPLY [E1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &fakeProvider{reply: "API-REPLY"}
			rec := &Recorder{Inner: inner, Dir: dir}
			got := streamAll(context.Background(), rec, tt.req)
			if got.err != nil || got.text != tt.want {
				t.Errorf("text = %q err=%v, want %q", got.text, got.err, tt.want)
			}
			if inner.callCount() != 0 {
				t.Errorf("inner called %d times on a hit, want 0", inner.callCount())
			}
		})
	}

	if after := readStoredBytes(t, dir, "oom-demo.json"); string(after) != string(before) {
		t.Errorf("file changed on a hit:\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestRecorderHitReturnsRecordedUsage(t *testing.T) {
	dir := recordDirWith(t, oomRecording())
	inner := &fakeProvider{reply: "API-REPLY"}
	got := streamAll(context.Background(), &Recorder{Inner: inner, Dir: dir}, oomRequest("oom-demo", 0))
	want := Usage{InputTokens: 812, OutputTokens: 96}
	if got.err != nil || got.text != recordedReply || got.usage != want {
		t.Errorf("got text=%q usage=%+v err=%v, want recorded reply and %+v", got.text, got.usage, got.err, want)
	}
}

func TestRecorderHitStopsOnDeltaError(t *testing.T) {
	dir := recordDirWith(t, matchRecording())
	inner := &fakeProvider{reply: "API-REPLY"}
	stop := errors.New("client went away")
	_, err := (&Recorder{Inner: inner, Dir: dir}).Stream(context.Background(), oomRequest("oom-demo", 0), func(string) error {
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("err = %v, want %v", err, stop)
	}
	if inner.callCount() != 0 {
		t.Errorf("inner called %d times, want 0", inner.callCount())
	}
}

func TestRecorderMissCallsInnerAndRecords(t *testing.T) {
	dir := recordDirWith(t, oomRecording())
	inner := &fakeProvider{reply: "NEW-REPLY [E1]", usage: Usage{InputTokens: 40, OutputTokens: 7}}
	rec := &Recorder{Inner: inner, Dir: dir}

	got := streamAll(context.Background(), rec, chatRequest(1, "你是怎么死的？"))
	if got.err != nil || got.text != "NEW-REPLY [E1]" {
		t.Fatalf("text = %q err=%v, want the inner reply", got.text, got.err)
	}
	if inner.callCount() != 1 {
		t.Fatalf("inner called %d times on a miss, want 1", inner.callCount())
	}

	stored := loadStoredRecording(t, dir)
	if len(stored.Turns) != 2 {
		t.Fatalf("file has %d turns, want 2: %+v", len(stored.Turns), stored.Turns)
	}
	added := stored.Turns[1]
	if added.TurnIndex != 1 || added.LastUser != "你是怎么死的？" || added.Reply != "NEW-REPLY [E1]" ||
		added.Usage != (recordedUsage{InputTokens: 40, OutputTokens: 7}) {
		t.Errorf("recorded turn = %+v", added)
	}
}

func TestRecorderMissCreatesFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	inner := &fakeProvider{reply: "FIRST"}
	got := streamAll(context.Background(), &Recorder{Inner: inner, Dir: dir}, oomRequest("oom-demo", 0))
	if got.err != nil || got.text != "FIRST" || inner.callCount() != 1 {
		t.Fatalf("text=%q err=%v calls=%d, want one call with FIRST", got.text, got.err, inner.callCount())
	}
	if rec := loadStoredRecording(t, dir); len(rec.Turns) != 1 || rec.Turns[0].Reply != "FIRST" {
		t.Errorf("stored turns = %+v", rec.Turns)
	}
}

func TestRecorderMissKeepsExistingTurns(t *testing.T) {
	existing := recording{
		FixtureID: "oom-demo",
		Turns: []recordedTurn{
			{TurnIndex: 0, Cause: "OOMKilled", LastUser: "opening", Reply: "OPENING", Usage: recordedUsage{InputTokens: 1, OutputTokens: 2}},
			{TurnIndex: 2, Cause: "OOMKilled", LastUser: "谁删了你？", Reply: "DELETE", Usage: recordedUsage{InputTokens: 3, OutputTokens: 4}},
		},
	}
	dir := recordDirWith(t, existing)
	inner := &fakeProvider{reply: "NEW"}

	if got := streamAll(context.Background(), &Recorder{Inner: inner, Dir: dir}, chatRequest(1, "怎么修复？")); got.err != nil {
		t.Fatal(got.err)
	}

	stored := loadStoredRecording(t, dir)
	if len(stored.Turns) != 3 {
		t.Fatalf("file has %d turns, want 3: %+v", len(stored.Turns), stored.Turns)
	}
	if stored.Turns[0] != existing.Turns[0] || stored.Turns[2] != existing.Turns[1] {
		t.Errorf("existing turns changed: %+v", stored.Turns)
	}
	if stored.Turns[1].TurnIndex != 1 || stored.Turns[1].Reply != "NEW" {
		t.Errorf("new turn = %+v", stored.Turns[1])
	}
}

func TestRecorderSecondCallInSameProcessHits(t *testing.T) {
	dir := t.TempDir()
	inner := &fakeProvider{reply: "ONCE"}
	rec := &Recorder{Inner: inner, Dir: dir}

	first := streamAll(context.Background(), rec, chatRequest(1, "怎么修复？"))
	second := streamAll(context.Background(), rec, chatRequest(3, "怎么修复？"))
	if first.err != nil || second.err != nil || second.text != "ONCE" {
		t.Fatalf("first err=%v, second text=%q err=%v", first.err, second.text, second.err)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1 (second request must hit the turn saved by the first)", inner.callCount())
	}
}

func TestRecorderDoesNotRecordEmptyReply(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{"empty", ""},
		{"spaces", "   "},
		{"newlines and tabs", "\n\t \n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			inner := &fakeProvider{reply: tt.reply}
			_, err := (&Recorder{Inner: inner, Dir: dir}).Stream(context.Background(), oomRequest("oom-demo", 0), noDelta)
			if !errors.Is(err, errEmptyReply) {
				t.Errorf("err = %v, want errEmptyReply", err)
			}
			if _, statErr := os.Stat(filepath.Join(dir, "oom-demo.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("file written for an empty reply (stat err %v)", statErr)
			}
		})
	}
}

func TestRecorderEmptyReplyKeepsExistingFile(t *testing.T) {
	dir := recordDirWith(t, matchRecording())
	before := readStoredBytes(t, dir, "oom-demo.json")
	inner := &fakeProvider{reply: "  "}
	_, err := (&Recorder{Inner: inner, Dir: dir}).Stream(context.Background(), chatRequest(5, "一个新问题"), noDelta)
	if !errors.Is(err, errEmptyReply) {
		t.Errorf("err = %v, want errEmptyReply", err)
	}
	if after := readStoredBytes(t, dir, "oom-demo.json"); string(after) != string(before) {
		t.Errorf("file changed after an empty reply")
	}
}

func TestRecorderDoesNotRecordInnerError(t *testing.T) {
	boom := errors.New("upstream 500")

	t.Run("no file yet", func(t *testing.T) {
		dir := t.TempDir()
		inner := &fakeProvider{err: boom}
		_, err := (&Recorder{Inner: inner, Dir: dir}).Stream(context.Background(), oomRequest("oom-demo", 0), noDelta)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "oom-demo.json")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("file written after an error (stat err %v)", statErr)
		}
	})

	t.Run("existing file unchanged", func(t *testing.T) {
		dir := recordDirWith(t, matchRecording())
		before := readStoredBytes(t, dir, "oom-demo.json")
		inner := &fakeProvider{err: boom}
		_, err := (&Recorder{Inner: inner, Dir: dir}).Stream(context.Background(), chatRequest(5, "一个新问题"), noDelta)
		if !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
		if after := readStoredBytes(t, dir, "oom-demo.json"); string(after) != string(before) {
			t.Errorf("file changed after an error")
		}
	})
}

func TestRecorderEmptyDirMissesWithoutReadingCurrentDirectory(t *testing.T) {
	inner := &fakeProvider{reply: "API"}
	_, err := (&Recorder{Inner: inner}).Stream(context.Background(), oomRequest("oom-demo", 0), noDelta)
	if err == nil {
		t.Error("err = nil, want the empty-directory error from save")
	}
	if inner.callCount() != 1 {
		t.Errorf("inner called %d times, want 1", inner.callCount())
	}
}
