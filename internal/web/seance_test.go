package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
)

// The fixture spirit that dies in a CrashLoop, and its namespace.
const (
	crashPod = "restless-spirit-84fc8b7c4c-52nl8"
	crashNS  = "seance-demo"
)

// newSeanceServer returns a server over the fixtures, with the mock provider and the given limits.
func newSeanceServer(t *testing.T, limits llm.Limits) *Server {
	t.Helper()
	return newTestServer(t, Options{
		Fixtures:     NewFixtureSource(),
		FixturesOnly: true,
		Provider:     llm.MockProvider{},
		Limits:       limits,
	})
}

// send runs one request against srv and returns the recorded response.
func send(t *testing.T, srv *Server, method, target, contentType, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// createSeance starts a séance over the fixtures through the JSON API and returns the 201 body.
func createSeance(t *testing.T, srv *Server, ns, pod string) createResponse {
	t.Helper()
	body := fmt.Sprintf(`{"ns":%q,"pod":%q,"src":"fixtures"}`, ns, pod)
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status = %d, body %s", pod, rec.Code, rec.Body)
	}
	var res createResponse
	decodeJSON(t, rec.Body.Bytes(), &res)
	return res
}

// askJSON posts one question as JSON and returns the response.
func askJSON(t *testing.T, srv *Server, id, text string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	return send(t, srv, http.MethodPost, "/api/seances/"+id+"/messages", "application/json", string(body), nil)
}

// decodeJSON decodes data into v and fails the test on error.
func decodeJSON(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

// sseMsg is one parsed event of a text/event-stream body.
type sseMsg struct {
	Name string
	Data string
}

// parseSSE splits a finished stream body into events. Comment lines are skipped.
func parseSSE(body string) []sseMsg {
	var out []sseMsg
	var cur sseMsg
	for _, line := range strings.Split(body, "\n") {
		switch {
		case line == "":
			if cur.Name != "" {
				out = append(out, cur)
			}
			cur = sseMsg{}
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

// streamSSE reads events from r and sends them to out until r ends or ctx is done.
func streamSSE(ctx context.Context, r io.Reader, out chan<- sseMsg) {
	sc := bufio.NewScanner(r)
	var cur sseMsg
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" {
				select {
				case out <- cur:
				case <-ctx.Done():
					return
				}
			}
			cur = sseMsg{}
		case strings.HasPrefix(line, "event: "):
			cur.Name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// turnsOf returns the decoded turn events of a stream.
func turnsOf(t *testing.T, msgs []sseMsg) []turnPayload {
	t.Helper()
	var out []turnPayload
	for _, m := range msgs {
		if m.Name != eventTurn {
			continue
		}
		var p turnPayload
		decodeJSON(t, []byte(m.Data), &p)
		out = append(out, p)
	}
	return out
}

// streamOnce opens ?once=1 and returns the parsed events.
func streamOnce(t *testing.T, srv *Server, id string) []sseMsg {
	t.Helper()
	rec := send(t, srv, http.MethodGet, "/api/seances/"+id+"/stream?once=1", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream once: status = %d", rec.Code)
	}
	return parseSSE(rec.Body.String())
}

// detailOf returns GET /api/seances/{id}.
func detailOf(t *testing.T, srv *Server, id string) seanceDetail {
	t.Helper()
	rec := send(t, srv, http.MethodGet, "/api/seances/"+id, "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status = %d, body %s", rec.Code, rec.Body)
	}
	var d seanceDetail
	decodeJSON(t, rec.Body.Bytes(), &d)
	return d
}

// hasHan reports whether s has a CJK ideograph.
func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func TestCreateSeanceJSON(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	if len(res.ID) != 32 {
		t.Errorf("id = %q, want 32 hex characters", res.ID)
	}
	if res.Pod != crashPod || res.Cause != "CrashLoopBackOff" {
		t.Errorf("pod/cause = %q/%q", res.Pod, res.Cause)
	}
	if res.StreamURL != "/api/seances/"+res.ID+"/stream" {
		t.Errorf("streamUrl = %q", res.StreamURL)
	}
	if res.PageURL != "/seances/"+res.ID {
		t.Errorf("pageUrl = %q", res.PageURL)
	}
}

func TestCreateSeanceFormRedirects(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	form := "ns=" + crashNS + "&pod=" + crashPod + "&src=fixtures"
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/x-www-form-urlencoded", form, nil)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/seances/") || len(loc) != len("/seances/")+32 {
		t.Errorf("Location = %q, want /seances/<id>", loc)
	}
}

func TestCreateSeanceErrors(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	tests := []struct {
		name string
		body string
		want int
	}{
		{"unknown pod", `{"ns":"seance-demo","pod":"no-such-pod","src":"fixtures"}`, http.StatusNotFound},
		{"missing pod", `{"ns":"seance-demo","src":"fixtures"}`, http.StatusBadRequest},
		{"bad namespace", `{"ns":"Bad_NS","pod":"x","src":"fixtures"}`, http.StatusBadRequest},
		{"unknown src", `{"ns":"seance-demo","pod":"x","src":"moon"}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", tt.body, nil)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestCreateSeanceClusterDown(t *testing.T) {
	// Without a cluster and without fixtures-only mode, a cluster request must say why it cannot run.
	srv := newTestServer(t, Options{Fixtures: NewFixtureSource(), ClusterErr: errors.New("no kubeconfig")})
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", `{"ns":"seance-demo","pod":"x"}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body %s", rec.Code, rec.Body)
	}
}

func TestStreamOnceOpeningAndReplay(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := send(t, srv, http.MethodGet, res.StreamURL+"?once=1", "", "", nil)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}
	first := turnsOf(t, parseSSE(rec.Body.String()))
	if len(first) != 1 {
		t.Fatalf("turn events = %d, want 1 (the opening); body:\n%s", len(first), rec.Body)
	}
	open := first[0]
	if open.Index != 0 || open.Role != "spirit" {
		t.Errorf("opening index/role = %d/%q", open.Index, open.Role)
	}
	if !strings.Contains(open.Content, "[E") {
		t.Errorf("opening has no [E#] reference: %q", open.Content)
	}
	if !strings.Contains(open.HTML, `class="ev-link" href="#ev-E`) {
		t.Errorf("opening html has no evidence link: %s", open.HTML)
	}

	// A second connection replays the same opening and does not spend the budget again.
	second := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(second) != 1 || second[0].Content != open.Content {
		t.Errorf("replay = %+v, want the same opening", second)
	}
	if d := detailOf(t, srv, res.ID); d.BudgetUsed != 1 {
		t.Errorf("budgetUsed after replay = %d, want 1", d.BudgetUsed)
	}
}

func TestConcurrentSubscribersOpenOnce(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	const n = 8
	bodies := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := send(t, srv, http.MethodGet, res.StreamURL+"?once=1", "", "", nil)
			bodies[i] = rec.Body.String()
		}()
	}
	wg.Wait()

	for i, b := range bodies {
		turns := turnsOf(t, parseSSE(b))
		if len(turns) != 1 || turns[0].Index != 0 {
			t.Errorf("subscriber %d saw %d turns, want only the opening", i, len(turns))
		}
	}
	if d := detailOf(t, srv, res.ID); d.BudgetUsed != 1 {
		t.Errorf("budgetUsed = %d, want 1 (opening once)", d.BudgetUsed)
	}
}

func TestAskHowToFix(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := askJSON(t, srv, res.ID, "怎么修复？")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var reply messageReply
	decodeJSON(t, rec.Body.Bytes(), &reply)
	if !strings.Contains(reply.Reply, "建议") || !strings.Contains(reply.Reply, "[E") {
		t.Errorf("reply = %q, want advice with an [E#] reference", reply.Reply)
	}
	if reply.Index != 2 {
		t.Errorf("index = %d, want 2 (opening 0, question 1, reply 2)", reply.Index)
	}
	if reply.Exhausted || reply.BudgetUsed != 2 {
		t.Errorf("exhausted/budgetUsed = %v/%d, want false/2", reply.Exhausted, reply.BudgetUsed)
	}

	// The stored reply carries the advice class on its turn.
	turns := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(turns) != 3 {
		t.Fatalf("turns = %d, want 3", len(turns))
	}
	if turns[1].Role != "user" || turns[1].Content != "怎么修复？" {
		t.Errorf("turn 1 = %+v, want the question", turns[1])
	}
	if !strings.Contains(turns[2].HTML, "turn-advice") {
		t.Errorf("reply html has no turn-advice class: %s", turns[2].HTML)
	}
}

func TestAskLanguageFollowsQuestion(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := askJSON(t, srv, res.ID, "How did you die?")
	var reply messageReply
	decodeJSON(t, rec.Body.Bytes(), &reply)
	if hasHan(reply.Reply) {
		t.Errorf("English question got a Chinese reply: %q", reply.Reply)
	}
	if !strings.Contains(reply.Reply, "[E") {
		t.Errorf("reply = %q, want an [E#] reference", reply.Reply)
	}
}

func TestAskOutsideEvidence(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := askJSON(t, srv, res.ID, "今天天气怎么样？")
	var reply messageReply
	decodeJSON(t, rec.Body.Bytes(), &reply)
	if !strings.Contains(reply.Reply, "冥界没有留下记录") {
		t.Errorf("reply = %q, want 冥界没有留下记录", reply.Reply)
	}
}

func TestMessageValidation(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	tests := []struct {
		name string
		text string
	}{
		{"blank", "   "},
		{"too long", strings.Repeat("问", maxQuestionRunes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := askJSON(t, srv, res.ID, tt.text)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestBudgetExhaustedAfterLimit(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{MaxCalls: 2})
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID) // call 1: the opening

	var ok messageReply
	decodeJSON(t, askJSON(t, srv, res.ID, "怎么修复？").Body.Bytes(), &ok) // call 2
	if ok.Exhausted || ok.BudgetRemaining != 0 {
		t.Fatalf("second call: exhausted=%v remaining=%d, want false/0", ok.Exhausted, ok.BudgetRemaining)
	}

	// A live subscriber must see the exhausted event before the reply turn.
	se := srv.seances.get(res.ID)
	sub, _, _ := se.subscribe()
	defer se.unsubscribe(sub)

	rec := askJSON(t, srv, res.ID, "怎么修复？")
	if rec.Code != http.StatusOK {
		t.Fatalf("exhausted ask: status = %d, want 200", rec.Code)
	}
	var out messageReply
	decodeJSON(t, rec.Body.Bytes(), &out)
	if !out.Exhausted {
		t.Error("exhausted = false, want true")
	}
	if out.Reply != llm.ExhaustedLine {
		t.Errorf("reply = %q, want ExhaustedLine", out.Reply)
	}
	if out.BudgetUsed != 2 {
		t.Errorf("budgetUsed = %d, want 2 (refused calls are not counted)", out.BudgetUsed)
	}
	if d := detailOf(t, srv, res.ID); !d.Exhausted {
		t.Error("detail.exhausted = false, want true")
	}

	var names []string
	for len(sub.ch) > 0 {
		ev := <-sub.ch
		if ev.name != eventDelta {
			names = append(names, ev.name)
		}
	}
	wantTail := []string{eventTurn, eventExhausted, eventTurn} // question, exhausted event, reply
	if strings.Join(names, ",") != strings.Join(wantTail, ",") {
		t.Errorf("live events = %v, want %v", names, wantTail)
	}
}

func TestConcurrentQuestionsAreSerialized(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	var wg sync.WaitGroup
	indexes := make(chan int, 2)
	for _, q := range []string{"怎么修复？", "你是怎么死的？"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := askJSON(t, srv, res.ID, q)
			var reply messageReply
			if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
				t.Errorf("decode reply %q: %v", rec.Body, err)
			}
			indexes <- reply.Index
		}()
	}
	wg.Wait()
	close(indexes)

	got := map[int]bool{}
	for i := range indexes {
		got[i] = true
	}
	if !got[2] || !got[4] || len(got) != 2 {
		t.Errorf("reply indexes = %v, want {2, 4}", got)
	}
	turns := turnsOf(t, streamOnce(t, srv, res.ID))
	for i, tr := range turns {
		if tr.Index != i {
			t.Errorf("turn %d has index %d; the turn list is out of order", i, tr.Index)
		}
	}
}

func TestHTMXMessageReturnsFragments(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/messages",
		"application/x-www-form-urlencoded", "text=%E6%80%8E%E4%B9%88%E4%BF%AE%E5%A4%8D%EF%BC%9F",
		map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Count(body, `class="turn `) != 2 {
		t.Errorf("fragment has %d turns, want 2 (question and reply): %s", strings.Count(body, `class="turn `), body)
	}
	if !strings.Contains(body, `data-turn="1"`) || !strings.Contains(body, `data-turn="2"`) {
		t.Errorf("fragment turn indexes are wrong: %s", body)
	}
}

func TestPlainFormMessageRedirects(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)

	rec := send(t, srv, http.MethodPost, "/api/seances/"+res.ID+"/messages",
		"application/x-www-form-urlencoded", "text=%E6%80%8E%E4%B9%88%E4%BF%AE%E5%A4%8D%EF%BC%9F", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != res.PageURL {
		t.Errorf("status/Location = %d/%q, want 303 to %s", rec.Code, rec.Header().Get("Location"), res.PageURL)
	}
	if n := len(turnsOf(t, streamOnce(t, srv, res.ID))); n != 3 {
		t.Errorf("turns after form post = %d, want 3", n)
	}
}

func TestStreamReplaysCompletedTurns(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	askJSON(t, srv, res.ID, "怎么修复？")

	turns := turnsOf(t, streamOnce(t, srv, res.ID))
	if len(turns) != 3 {
		t.Fatalf("replayed turns = %d, want 3", len(turns))
	}
	for i, tr := range turns {
		if tr.Index != i {
			t.Errorf("replayed turn %d has index %d", i, tr.Index)
		}
	}
}

func TestLiveSubscriberGetsDeltasAndTurns(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	body := fmt.Sprintf(`{"ns":%q,"pod":%q,"src":"fixtures"}`, crashNS, crashPod)
	resp, err := http.Post(hs.URL+"/api/seances", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created createResponse
	decodeJSON(t, readAll(t, resp), &created)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, hs.URL+created.StreamURL, nil)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	events := make(chan sseMsg, 512)
	go streamSSE(ctx, stream.Body, events)

	waitFor := func(match func(sseMsg) bool) sseMsg {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case m := <-events:
				if match(m) {
					return m
				}
			case <-timeout:
				t.Fatal("timed out waiting for a stream event")
			}
		}
	}

	// The opening arrives first. Once it is in, the subscriber is registered for the next turns.
	waitFor(func(m sseMsg) bool { return m.Name == eventTurn && strings.Contains(m.Data, `"index":0`) })

	msgResp, err := http.Post(hs.URL+"/api/seances/"+created.ID+"/messages",
		"application/json", strings.NewReader(`{"text":"怎么修复？"}`))
	if err != nil {
		t.Fatal(err)
	}
	if msgResp.StatusCode != http.StatusOK {
		t.Fatalf("message status = %d", msgResp.StatusCode)
	}
	msgResp.Body.Close()

	deltas := 0
	for {
		m := waitFor(func(m sseMsg) bool { return m.Name == eventTurn || m.Name == eventDelta })
		if m.Name == eventDelta {
			if strings.Contains(m.Data, `"index":2`) {
				deltas++
			}
			continue
		}
		if strings.Contains(m.Data, `"index":2`) {
			var p turnPayload
			decodeJSON(t, []byte(m.Data), &p)
			if p.Role != "spirit" || !strings.Contains(p.Content, "建议") {
				t.Errorf("live reply = %+v", p)
			}
			break
		}
	}
	if deltas == 0 {
		t.Error("no delta events for the reply before its turn event")
	}
}

// readAll reads and closes a response body.
func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fixedSource serves one autopsy as the cluster. It lets a test change the logs.
type fixedSource struct{ a *autopsy.PodAutopsy }

func (fixedSource) Name() string { return clusterSource }

func (fixedSource) List(context.Context, string) ([]autopsy.SpiritSummary, error) { return nil, nil }

func (f fixedSource) Autopsy(_ context.Context, _, name string) (*autopsy.PodAutopsy, error) {
	if name != f.a.Name {
		return nil, ErrNotFound
	}
	return f.a, nil
}

func TestSeancePageListsEvidenceAndEscapes(t *testing.T) {
	a, err := NewFixtureSource().Autopsy(context.Background(), crashNS, crashPod)
	if err != nil {
		t.Fatal(err)
	}
	a.Containers[0].LogTail = append(a.Containers[0].LogTail, `FATAL <script>alert(1)</script> & "quoted"`)
	srv := newTestServer(t, Options{Cluster: fixedSource{a: a}, Provider: llm.MockProvider{}})

	body := fmt.Sprintf(`{"ns":%q,"pod":%q}`, crashNS, crashPod)
	rec := send(t, srv, http.MethodPost, "/api/seances", "application/json", body, nil)
	var created createResponse
	decodeJSON(t, rec.Body.Bytes(), &created)
	d := detailOf(t, srv, created.ID)
	if len(d.Evidence) == 0 {
		t.Fatal("no evidence collected")
	}

	page := send(t, srv, http.MethodGet, created.PageURL, "", "", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d", page.Code)
	}
	html := page.Body.String()
	for _, e := range d.Evidence {
		if !strings.Contains(html, `<details id="ev-`+e.ID+`">`) {
			t.Errorf("page has no evidence card for %s", e.ID)
		}
	}
	if n := strings.Count(html, `<details id="ev-`); n != len(d.Evidence) {
		t.Errorf("evidence cards = %d, want %d", n, len(d.Evidence))
	}
	if strings.Contains(html, "<script>alert(1)") {
		t.Error("evidence text was not escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("escaped evidence text not found on the page")
	}
	if !strings.Contains(html, `id="log"`) || !strings.Contains(html, `sse-connect="`+created.StreamURL+`"`) {
		t.Error("page does not connect to the stream")
	}
}

func TestSeancePageShowsOpeningAndTurns(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	res := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, res.ID)
	askJSON(t, srv, res.ID, "怎么修复？")

	page := send(t, srv, http.MethodGet, res.PageURL, "", "", nil)
	html := page.Body.String()
	if n := strings.Count(html, `data-turn="`); n != 3 {
		t.Errorf("page has %d turns, want 3", n)
	}
	if !strings.Contains(html, "CrashLoopBackOff") || !strings.Contains(html, crashPod) {
		t.Error("page header lacks the pod name or cause")
	}
	if !strings.Contains(html, "结束降灵会，生成复盘") {
		t.Error("postmortem placeholder missing")
	}
}

func TestUnknownSeanceIs404(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	tests := []struct {
		method, target, body string
	}{
		{http.MethodGet, "/api/seances/nope", ""},
		{http.MethodGet, "/api/seances/nope/stream?once=1", ""},
		{http.MethodPost, "/api/seances/nope/messages", `{"text":"hi"}`},
		{http.MethodGet, "/seances/nope", ""},
	}
	for _, tt := range tests {
		rec := send(t, srv, tt.method, tt.target, "application/json", tt.body, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", tt.method, tt.target, rec.Code)
		}
	}
}

func TestSeanceExpiresAfterIdleTTL(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{})
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	srv.seances.now = func() time.Time { return now }
	res := createSeance(t, srv, crashNS, crashPod)

	now = now.Add(seanceIdleTTL) // exactly 2h idle is still alive
	if rec := send(t, srv, http.MethodGet, "/api/seances/"+res.ID, "", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("at 2h idle: status = %d, want 200", rec.Code)
	}
	now = now.Add(seanceIdleTTL + time.Second)
	if rec := send(t, srv, http.MethodGet, "/api/seances/"+res.ID, "", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("after 2h idle: status = %d, want 404", rec.Code)
	}
}

func TestSeanceBudgetsAreSeparate(t *testing.T) {
	srv := newSeanceServer(t, llm.Limits{MaxCalls: 2})
	a := createSeance(t, srv, crashNS, crashPod)
	b := createSeance(t, srv, crashNS, crashPod)
	streamOnce(t, srv, a.ID)
	askJSON(t, srv, a.ID, "怎么修复？")

	if d := detailOf(t, srv, a.ID); d.BudgetUsed != 2 || !d.Exhausted {
		t.Errorf("séance a: used=%d exhausted=%v, want 2/true", d.BudgetUsed, d.Exhausted)
	}
	if d := detailOf(t, srv, b.ID); d.BudgetUsed != 0 || d.Exhausted {
		t.Errorf("séance b: used=%d exhausted=%v, want 0/false", d.BudgetUsed, d.Exhausted)
	}
}
