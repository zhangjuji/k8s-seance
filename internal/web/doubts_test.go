package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// liarReply contradicts the crashloop fixture: there is no E99, the limit is
// 32Mi, the exit code is 1 and the Pod restarted 8 times. The <b> checks escaping.
const liarReply = "我死于 exitCode 137 [E99]，重启 3 次，上限 512Mi <b>x</b>。"

// liarProvider answers every request with a fixed text.
type liarProvider struct{ text string }

func (p liarProvider) Stream(_ context.Context, _ llm.ChatRequest, onDelta func(string) error) (llm.Usage, error) {
	return llm.Usage{}, onDelta(p.text)
}

func newLiarServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, Options{
		Fixtures:     NewFixtureSource(),
		FixturesOnly: true,
		Provider:     liarProvider{text: liarReply},
	})
}

func doubtKinds(ds []doubtItem) map[string]bool {
	out := make(map[string]bool)
	for _, d := range ds {
		out[d.Kind] = true
	}
	return out
}

func wantLiarDoubts(t *testing.T, where string, ds []doubtItem) {
	t.Helper()
	kinds := doubtKinds(ds)
	for _, k := range []string{"citation", "exitCode", "restarts", "memory"} {
		if !kinds[k] {
			t.Errorf("%s: no %s doubt in %+v", where, k, ds)
		}
	}
	for _, d := range ds {
		if d.Message == "" || d.Claim == "" || d.Sentence == "" {
			t.Errorf("%s: incomplete doubt %+v", where, d)
		}
	}
}

func TestDoubtsInJSONAndStream(t *testing.T) {
	srv := newLiarServer(t)
	res := createSeance(t, srv, crashNS, crashPod)

	// The opening (turn 0) on the stream carries doubts.
	turns := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(turns) != 1 {
		t.Fatalf("stream turns = %d, want 1", len(turns))
	}
	wantLiarDoubts(t, "sse opening", turns[0].Doubts)
	if !strings.Contains(turns[0].HTML, "亡灵可能在说谎") || !strings.Contains(turns[0].HTML, `data-doubts="4"`) {
		t.Errorf("opening html lacks the doubt box: %s", turns[0].HTML)
	}

	// The message reply carries doubts.
	rec := askJSON(t, srv, res.ID, "你是怎么死的？")
	if rec.Code != http.StatusOK {
		t.Fatalf("ask: status = %d, body %s", rec.Code, rec.Body)
	}
	var reply messageReply
	decodeJSON(t, rec.Body.Bytes(), &reply)
	wantLiarDoubts(t, "message reply", reply.Doubts)

	// The séance detail carries doubts per turn: spirit turns have them, the user turn has [].
	d := detailOf(t, srv, res.ID)
	if len(d.Transcript) != 3 {
		t.Fatalf("transcript = %d turns, want 3", len(d.Transcript))
	}
	wantLiarDoubts(t, "detail opening", d.Transcript[0].Doubts)
	wantLiarDoubts(t, "detail reply", d.Transcript[2].Doubts)
	if d.Transcript[1].Role != roleUser || len(d.Transcript[1].Doubts) != 0 {
		t.Errorf("user turn should have no doubts: %+v", d.Transcript[1])
	}

	// Replayed turns on a new stream keep their doubts.
	again := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(again) != 3 {
		t.Fatalf("replayed turns = %d, want 3", len(again))
	}
	wantLiarDoubts(t, "sse replay", again[2].Doubts)
}

func TestDoubtsRenderedEscapedUnderReply(t *testing.T) {
	srv := newLiarServer(t)
	res := createSeance(t, srv, crashNS, crashPod)
	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/messages",
		"application/x-www-form-urlencoded", "text=hi", map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("htmx ask: status = %d", rec.Code)
	}
	html := rec.Body.String()
	for _, want := range []string{
		`class="doubts"`, "亡灵可能在说谎", "引用 [E99] 不在证据列表里", "内存数量 512Mi 在证据里找不到",
		"不表示已验证", `turn-doubted`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("htmx fragment lacks %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "<b>x</b>") {
		t.Error("reply text was not escaped")
	}
	// The doubt box sits inside the spirit turn, after its text.
	spirit := html[strings.Index(html, `turn-spirit`):]
	if i, j := strings.Index(spirit, `class="text"`), strings.Index(spirit, `class="doubts"`); i < 0 || j < i {
		t.Error("doubt box is not under the reply text")
	}
	// The page shows the same box for the stored turns.
	page := send(t, srv, http.MethodGet, res.PageURL, "", "", nil).Body.String()
	if !strings.Contains(page, "亡灵可能在说谎") {
		t.Error("séance page does not show the doubts")
	}
}

func TestDoubtsEnglishSeance(t *testing.T) {
	srv := newLiarServer(t)
	body := `{"ns":"` + crashNS + `","pod":"` + crashPod + `","src":"fixtures","lang":"en"}`
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, nil)
	var created createResponse
	decodeJSON(t, rec.Body.Bytes(), &created)
	turns := turnsOf(t, streamOnce(t, srv, created.ID))
	if len(turns) != 1 {
		t.Fatalf("turns = %d", len(turns))
	}
	if !strings.Contains(turns[0].HTML, "The spirit may be lying") {
		t.Errorf("english doubt title missing: %s", turns[0].HTML)
	}
	for _, d := range turns[0].Doubts {
		if hasHan(strings.ReplaceAll(d.Message, d.Claim, "")) {
			t.Errorf("english doubt message has Chinese text: %q", d.Message)
		}
	}
}

// TestNoDoubtsForMockSeance checks that the mock gives no doubts and that the
// doubts field is still present as [] in every JSON shape.
func TestNoDoubtsForMockSeance(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{MaxCalls: 3})
	res := createSeance(t, srv, crashNS, crashPod)
	msgs := streamOnce(t, srv, res.ID)
	for _, m := range msgs {
		if m.Name == eventTurn && !strings.Contains(m.Data, `"doubts":[]`) {
			t.Errorf("sse turn without empty doubts: %s", m.Data)
		}
	}
	for _, q := range []string{"怎么修复？", "你什么时候死的？", "How did you die?"} {
		rec := askJSON(t, srv, res.ID, q)
		if !strings.Contains(rec.Body.String(), `"doubts":[]`) {
			t.Errorf("%s: reply without empty doubts: %s", q, rec.Body)
		}
	}
	raw := send(t, srv, http.MethodGet, "/api/seances/"+res.ID, "", "", nil).Body.Bytes()
	var d struct {
		Transcript []map[string]json.RawMessage `json:"transcript"`
	}
	decodeJSON(t, raw, &d)
	for _, item := range d.Transcript {
		if string(item["doubts"]) != "[]" {
			t.Errorf("transcript item doubts = %s, want []", item["doubts"])
		}
	}
	page := send(t, srv, http.MethodGet, res.PageURL, "", "", nil).Body.String()
	if strings.Contains(page, `class="doubts"`) {
		t.Error("mock séance page shows a doubt box")
	}
}
