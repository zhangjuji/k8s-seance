package llm

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// lineageEvidence is the evidence of a spirit whose release broke it while an
// older revision lives on, as autopsy.BuildEvidence writes it.
func lineageEvidence(verdict, age string, alive int) []autopsy.Evidence {
	ev := []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container app state: waiting reason=CrashLoopBackOff; restarts: 4; lastState: terminated reason=Error exitCode=1"},
		{ID: "E2", Kind: "event", Text: "event Warning BackOff: Back-off restarting failed container (count 9)"},
		{ID: "E3", Kind: "lineage", Text: "family Deployment/second-life: verdict=" + verdict + " (x); this revision pod-template-hash=bbb: alive 0/1"},
	}
	life := "former life"
	if age == autopsy.AgeNewer {
		life = "later life"
	}
	ev = append(ev, autopsy.Evidence{ID: "E4", Kind: "lineage",
		Text: "revision pod-template-hash=aaa (" + age + ", " + life + "): alive " + strconv.Itoa(alive) + "/2; pods: a; differs from this revision: container app env MODE: \"normal\" -> \"turbo\""})
	return ev
}

func openingWith(t *testing.T, ev []autopsy.Evidence, lang string) string {
	t.Helper()
	chunks, _ := collect(t, context.Background(), MockProvider{}, ChatRequest{
		Purpose: PurposeOpening, Cause: autopsy.CauseCrashLoopBackOff, Evidence: ev, Lang: lang,
		Messages: userMsg("请做开场陈述。"),
	})
	return strings.Join(chunks, "")
}

func TestMockOpeningMentionsLivingFormerLife(t *testing.T) {
	ev := lineageEvidence(autopsy.VerdictRegression, autopsy.AgeOlder, 2)
	zh := openingWith(t, ev, LangZH)
	if !strings.Contains(zh, "我的前世还活着") || !strings.Contains(zh, "[E4]") || !strings.Contains(zh, "先回滚") {
		t.Errorf("zh opening = %q, want the living former life cited as [E4]", zh)
	}
	en := openingWith(t, ev, LangEN)
	if !strings.Contains(en, "My former life is still alive") || !strings.Contains(en, "[E4]") || hasCJK(en) {
		t.Errorf("en opening = %q", en)
	}
}

func TestMockOpeningIgnoresOtherFamilies(t *testing.T) {
	cases := map[string][]autopsy.Evidence{
		"instance":         lineageEvidence(autopsy.VerdictInstance, autopsy.AgeOlder, 2),
		"family dead":      lineageEvidence(autopsy.VerdictFamily, autopsy.AgeOlder, 0),
		"newer life lives": lineageEvidence(autopsy.VerdictRegression, autopsy.AgeNewer, 2),
		"no lineage":       lineageEvidence(autopsy.VerdictRegression, autopsy.AgeOlder, 2)[:2],
	}
	for name, ev := range cases {
		for _, lang := range []string{LangZH, LangEN} {
			got := openingWith(t, ev, lang)
			if strings.Contains(got, "前世") || strings.Contains(got, "former life") {
				t.Errorf("%s/%s: opening mentions a former life: %q", name, lang, got)
			}
		}
	}
}
