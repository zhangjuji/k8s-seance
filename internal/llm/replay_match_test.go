package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

const (
	fixMsg    = "FIX-REPLY [E1]"
	deleteMsg = "DELETE-REPLY [E1]"
)

// matchRecording has an opening (turn 0), two follow-ups at turns 1 and 2, and a
// postmortem turn (100). Their LastUser values differ, so each one can be told apart.
func matchRecording() recording {
	return recording{
		FixtureID: "oom-demo",
		Turns: []recordedTurn{
			{TurnIndex: 0, LastUser: "请做开场陈述。", Reply: "OPENING-REPLY [E1]"},
			{TurnIndex: 1, LastUser: "怎么修复？", Reply: fixMsg},
			{TurnIndex: 2, LastUser: "谁删了你？", Reply: deleteMsg},
			{TurnIndex: PostmortemTurnIndex, LastUser: "postmortem prompt", Reply: "POSTMORTEM-REPLY [E1]"},
		},
	}
}

// chatRequest builds a follow-up request for fixture oom-demo at turnIndex, with
// question as the last user message.
func chatRequest(turnIndex int, question string) ChatRequest {
	req := oomRequest("oom-demo", turnIndex)
	req.Messages = []Message{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "请做开场陈述。"},
		{Role: "assistant", Content: "opening text"},
		{Role: "user", Content: question},
	}
	return req
}

func TestReplayFollowUpMatchesByQuestionNotIndex(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, matchRecording())}}

	tests := []struct {
		name      string
		turnIndex int
		question  string
		want      string
	}{
		{"same question at recorded index", 1, "怎么修复？", fixMsg},
		{"same question at a different index", 5, "怎么修复？", fixMsg},
		{"other recorded question at index 1", 1, "谁删了你？", deleteMsg},
		{"other recorded question at a later index", 9, "谁删了你？", deleteMsg},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallback := &fakeProvider{reply: "fallback"}
			p := &ReplayProvider{FS: fsys, Fallback: fallback}
			got := streamAll(context.Background(), p, chatRequest(tt.turnIndex, tt.question))
			if got.err != nil || got.text != tt.want {
				t.Errorf("text = %q err=%v, want %q", got.text, got.err, tt.want)
			}
			if fallback.callCount() != 0 {
				t.Errorf("fallback called %d times on a hit", fallback.callCount())
			}
		})
	}
}

func TestReplayDifferentQuestionMissesToMock(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, matchRecording())}}

	tests := []struct {
		name      string
		turnIndex int
		question  string
	}{
		{"unrecorded question at recorded index", 1, "你是怎么死的？"},
		{"unrecorded question at other index", 3, "你昨天晚饭吃了什么？"},
		{"question differs by a word", 1, "怎么修复得更快？"},
		{"English question", 2, "How do I fix you?"},
		{"empty question", 1, ""},
		{"whitespace-only question", 1, "   "},
		{"question matches only the opening", 2, "请做开场陈述。"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := chatRequest(tt.turnIndex, tt.question)
			want := streamAll(context.Background(), MockProvider{}, req)
			got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
			if got.err != nil {
				t.Fatalf("Stream: %v", got.err)
			}
			if got.text != want.text {
				t.Errorf("text = %q, want mock output %q", got.text, want.text)
			}
		})
	}
}

func TestReplayQuestionVariantsHit(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, matchRecording())}}

	tests := []struct {
		name     string
		question string
	}{
		{"ASCII question mark", "怎么修复?"},
		{"no question mark", "怎么修复"},
		{"surrounding spaces", "  怎么修复？  "},
		{"space before full-width mark", "怎么修复 ？"},
		{"full-width exclamation", "怎么修复！"},
		{"ASCII exclamation", "怎么修复!"},
		{"full-width full stop", "怎么修复。"},
		{"full-width comma tail", "怎么修复，"},
		{"repeated trailing marks", "怎么修复??"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallback := &fakeProvider{reply: "fallback"}
			p := &ReplayProvider{FS: fsys, Fallback: fallback}
			got := streamAll(context.Background(), p, chatRequest(4, tt.question))
			if got.err != nil || got.text != fixMsg {
				t.Errorf("text = %q err=%v, want recorded %q", got.text, got.err, fixMsg)
			}
			if fallback.callCount() != 0 {
				t.Errorf("fallback called %d times, want 0", fallback.callCount())
			}
		})
	}
}

func TestReplayASCIICaseIsIgnored(t *testing.T) {
	rec := recording{
		FixtureID: "oom-demo",
		Turns:     []recordedTurn{{TurnIndex: 1, LastUser: "How do I fix you?", Reply: "EN-REPLY [E1]"}},
	}
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}

	for _, q := range []string{"How do I fix you?", "how do i fix you", "HOW DO I FIX YOU?"} {
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, chatRequest(2, q))
		if got.err != nil || got.text != "EN-REPLY [E1]" {
			t.Errorf("question %q: text = %q err=%v, want recorded reply", q, got.text, got.err)
		}
	}
}

func TestReplayLowestRecordedIndexWinsWhenQuestionRepeats(t *testing.T) {
	rec := recording{
		FixtureID: "oom-demo",
		Turns: []recordedTurn{
			{TurnIndex: 3, LastUser: "怎么修复？", Reply: "later"},
			{TurnIndex: 1, LastUser: "怎么修复？", Reply: "first"},
		},
	}
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}
	got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, chatRequest(7, "怎么修复？"))
	if got.text != "first" {
		t.Errorf("text = %q, want the reply at the lowest recorded index", got.text)
	}
}

func TestReplayOpeningMatchesByIndexOnly(t *testing.T) {
	// The opening recording's LastUser is not compared. A turn-1 recording whose
	// LastUser equals the opening's question must not answer the opening.
	rec := recording{
		FixtureID: "oom-demo",
		Turns: []recordedTurn{
			{TurnIndex: 0, LastUser: "请做开场陈述。", Reply: "OPENING-REPLY"},
			{TurnIndex: 1, LastUser: "opening", Reply: "FOLLOW-UP-REPLY"},
		},
	}
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}

	t.Run("opening matched by index though its question differs", func(t *testing.T) {
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, oomRequest("oom-demo", 0))
		if got.text != "OPENING-REPLY" {
			t.Errorf("text = %q, want OPENING-REPLY", got.text)
		}
	})

	t.Run("opening index does not fall back to a follow-up question", func(t *testing.T) {
		rec := recording{
			FixtureID: "oom-demo",
			Turns:     []recordedTurn{{TurnIndex: 1, LastUser: "opening", Reply: "FOLLOW-UP-REPLY"}},
		}
		fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}
		req := oomRequest("oom-demo", 0)
		want := streamAll(context.Background(), MockProvider{}, req)
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
		if got.text != want.text {
			t.Errorf("text = %q, want mock output %q", got.text, want.text)
		}
	})

	t.Run("follow-up does not replay the opening turn", func(t *testing.T) {
		fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}
		req := chatRequest(2, "请做开场陈述。")
		want := streamAll(context.Background(), MockProvider{}, req)
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
		if got.text != want.text {
			t.Errorf("text = %q, want mock output %q", got.text, want.text)
		}
	})
}

func TestReplayPostmortemMatchesByIndexOnly(t *testing.T) {
	fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, matchRecording())}}
	const postmortemReply = "POSTMORTEM-REPLY [E1]"

	t.Run("any messages hit the postmortem recording", func(t *testing.T) {
		req := chatRequest(PostmortemTurnIndex, "a transcript the recording never saw")
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
		if got.err != nil || got.text != postmortemReply {
			t.Errorf("text = %q err=%v, want %q", got.text, got.err, postmortemReply)
		}
	})

	t.Run("postmortem index is not used as a follow-up match", func(t *testing.T) {
		rec := recording{
			FixtureID: "oom-demo",
			Turns:     []recordedTurn{{TurnIndex: PostmortemTurnIndex, LastUser: "怎么修复？", Reply: postmortemReply}},
		}
		fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}
		req := chatRequest(3, "怎么修复？")
		want := streamAll(context.Background(), MockProvider{}, req)
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
		if got.text != want.text {
			t.Errorf("text = %q, want mock output %q", got.text, want.text)
		}
	})

	t.Run("postmortem request with no matching index misses", func(t *testing.T) {
		req := chatRequest(PostmortemTurnIndex, "x")
		rec := recording{FixtureID: "oom-demo", Turns: []recordedTurn{{TurnIndex: 1, LastUser: "x", Reply: "FOLLOW-UP"}}}
		fsys := fstest.MapFS{"oom-demo.json": {Data: mustJSON(t, rec)}}
		want := streamAll(context.Background(), MockProvider{}, req)
		got := streamAll(context.Background(), &ReplayProvider{FS: fsys}, req)
		if got.text != want.text {
			t.Errorf("text = %q, want mock output %q", got.text, want.text)
		}
	})
}

func TestRecordedQuestionReplaysWhenAskedAgain(t *testing.T) {
	// A question recorded through the Recorder must match when it is asked again,
	// even at another index and with different spacing or punctuation.
	dir := t.TempDir()
	rec := &Recorder{Inner: &fakeProvider{reply: "recorded fix"}, Dir: dir}
	req := oomRequest("oom-demo", 1)
	req.Messages = []Message{{Role: "user", Content: "怎么修复？"}}
	if _, err := rec.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}

	fallback := &fakeProvider{reply: "fallback"}
	p := &ReplayProvider{FS: fstest.MapFS{"oom-demo.json": {Data: readStoredBytes(t, dir, "oom-demo.json")}}, Fallback: fallback}
	for _, q := range []string{"怎么修复？", "怎么修复", " 怎么修复 ?"} {
		req := oomRequest("oom-demo", 4)
		req.Messages = []Message{{Role: "user", Content: q}}
		got := streamAll(context.Background(), p, req)
		if got.text != "recorded fix" {
			t.Errorf("question %q: text = %q, want recorded fix", q, got.text)
		}
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback called %d times, want 0", fallback.callCount())
	}
}

func TestNormalizeQuestion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"怎么修复？", "怎么修复"},
		{"怎么修复?", "怎么修复"},
		{"  怎么修复 ？  ", "怎么修复"},
		{"How do I fix you?", "how do i fix you"},
		{"Ok!?.,", "ok"},
		{"中 间 空格", "中 间 空格"},
		{"", ""},
		{"？", ""},
	}
	for _, tt := range tests {
		if got := normalizeQuestion(tt.in); got != tt.want {
			t.Errorf("normalizeQuestion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// readStoredBytes returns the content of a file the Recorder wrote into dir.
func readStoredBytes(t *testing.T, dir, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}
