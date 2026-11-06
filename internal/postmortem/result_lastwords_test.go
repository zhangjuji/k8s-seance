package postmortem

import (
	"context"
	"strings"
	"testing"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/spirit"
)

// TestResultLastWordsIsPlainQuote checks that Result.LastWords is the line quoted in
// the "> Last words" row, without the Markdown escaping the report applies.
func TestResultLastWordsIsPlainQuote(t *testing.T) {
	all, err := fixtures.Load()
	if err != nil {
		t.Fatal(err)
	}
	a := all[0]
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		transcript []spirit.Turn
		want       string
	}{
		{"fallback when no spirit line", nil, lastWordsFallback[autopsy.Classify(a).Cause]},
		{"spirit line, markdown stripped", []spirit.Turn{{Role: "spirit", Content: "我的 *内存* 涨满了。"}}, "我的 内存 涨满了。"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Generate(context.Background(), Input{
				Autopsy:    a,
				Class:      autopsy.Classify(a),
				Evidence:   ev,
				Transcript: tc.transcript,
			}, llm.MockProvider{})
			if err != nil {
				t.Fatal(err)
			}
			if res.LastWords != tc.want {
				t.Errorf("LastWords = %q, want %q", res.LastWords, tc.want)
			}
			if !strings.Contains(res.Markdown, "> Last words: \""+escapeMD(tc.want)+"\"") {
				t.Errorf("markdown does not quote %q:\n%s", escapeMD(tc.want), res.Markdown)
			}
		})
	}
}
