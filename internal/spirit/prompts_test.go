package spirit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// causeConstants reads internal/autopsy/model.go and returns the value of every
// constant declared with type CauseOfDeath. Parsing the source means a new cause
// constant without a voice fails this test.
func causeConstants(t *testing.T) []autopsy.CauseOfDeath {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "../autopsy/model.go", nil, 0)
	if err != nil {
		t.Fatalf("parse model.go: %v", err)
	}
	var out []autopsy.CauseOfDeath
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "CauseOfDeath" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("cause constant value is not a literal")
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				out = append(out, autopsy.CauseOfDeath(s))
			}
		}
	}
	return out
}

func TestEveryCauseHasVoice(t *testing.T) {
	causes := causeConstants(t)
	if len(causes) == 0 {
		t.Fatal("no CauseOfDeath constants found in model.go")
	}
	for _, c := range causes {
		t.Run(string(c), func(t *testing.T) {
			if _, ok := voiceFiles[c]; !ok {
				t.Fatalf("cause %q has no entry in voiceFiles", c)
			}
			if v := Voice(c); v == "" {
				t.Fatalf("Voice(%q) is empty", c)
			}
		})
	}
}

func TestVoicePhrases(t *testing.T) {
	tests := []struct {
		cause  autopsy.CauseOfDeath
		phrase string
	}{
		{autopsy.CauseOOMKilled, "窒息、越来越重"},
		{autopsy.CauseCrashLoopBackOff, "一次次复活又倒下"},
		{autopsy.CauseImagePullBackOff, "我从未真正活过"},
		{autopsy.CauseProbeFailure, "他们以为我死了，于是真的杀了我"},
	}
	for _, tt := range tests {
		t.Run(string(tt.cause), func(t *testing.T) {
			if got := Voice(tt.cause); !strings.Contains(got, tt.phrase) {
				t.Fatalf("Voice(%s) does not contain %q:\n%s", tt.cause, tt.phrase, got)
			}
		})
	}
}

func TestVoiceFallback(t *testing.T) {
	want := Voice(autopsy.CauseUnknown)
	if want == "" {
		t.Fatal("unknown voice is empty")
	}
	for _, c := range []autopsy.CauseOfDeath{"", "NoSuchCause", "../persona"} {
		t.Run(string(c), func(t *testing.T) {
			if got := Voice(c); got != want {
				t.Fatalf("Voice(%q) = %q, want unknown voice", c, got)
			}
		})
	}
}

func TestSystemPrompt(t *testing.T) {
	deathTime := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	evidence := []autopsy.Evidence{
		{ID: "E1", Kind: "status", Text: "container memory-glutton terminated: OOMKilled, exit code 137"},
		{ID: "E2", Kind: "event", Text: "Warning BackOff: restarting failed container"},
		{ID: "E3", Kind: "log", Text: "line one\nline two with a newline"},
	}
	tests := []struct {
		name     string
		data     PromptData
		contains []string
	}{
		{
			name: "oom with evidence",
			data: PromptData{
				PodName:   "memory-glutton",
				Namespace: "seance-demo",
				Cause:     autopsy.CauseOOMKilled,
				DeathTime: deathTime.Format(time.RFC3339),
				Evidence:  evidence,
			},
			contains: []string{
				"memory-glutton", "seance-demo", "OOMKilled", deathTime.Format(time.RFC3339),
				"窒息、越来越重", "[E1] (status) container memory-glutton terminated",
				"[E2] (event)", "[E3] (log) line one line two with a newline",
				"冥界没有留下记录",
			},
		},
		{
			name: "crashloop",
			data: PromptData{
				PodName: "restless-spirit", Namespace: "seance-demo",
				Cause: autopsy.CauseCrashLoopBackOff, Evidence: evidence,
			},
			contains: []string{"restless-spirit", "CrashLoopBackOff", "一次次复活又倒下", "冥界没有留下记录"},
		},
		{
			name: "no evidence and no death time",
			data: PromptData{PodName: "lost-soul", Namespace: "default", Cause: autopsy.CauseImagePullBackOff},
			contains: []string{
				"lost-soul", "ImagePullBackOff", "我从未真正活过",
				"(none) 冥界没有留下记录", "time of death): 冥界没有留下记录",
			},
		},
		{
			name:     "empty cause uses unknown",
			data:     PromptData{PodName: "ghost", Namespace: "default", Evidence: evidence},
			contains: []string{"ghost", "Unknown", Voice(autopsy.CauseUnknown)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SystemPrompt(tt.data)
			if err != nil {
				t.Fatalf("SystemPrompt: %v", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(got, s) {
					t.Errorf("prompt missing %q", s)
				}
			}
			if strings.Contains(got, "{{") {
				t.Errorf("prompt has unrendered template markers")
			}
		})
	}
}

func TestSystemPromptContainsEveryEvidenceID(t *testing.T) {
	var ev []autopsy.Evidence
	for i := 1; i <= 12; i++ {
		ev = append(ev, autopsy.Evidence{ID: "E" + strconv.Itoa(i), Kind: "log", Text: "x"})
	}
	got, err := SystemPrompt(PromptData{PodName: "p", Namespace: "n", Cause: autopsy.CauseError, Evidence: ev})
	if err != nil {
		t.Fatalf("SystemPrompt: %v", err)
	}
	for _, e := range ev {
		if !strings.Contains(got, "["+e.ID+"] (log) x") {
			t.Errorf("prompt missing evidence line for %s", e.ID)
		}
	}
}
