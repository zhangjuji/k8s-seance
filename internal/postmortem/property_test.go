package postmortem

// Property test for transcript clipping (design.md "Testing Strategy",
// property P-H4): each séance turn sent to the model is cut by truncate.

import (
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"unicode/utf8"
)

// P-H4: truncate(s, n) returns s when it has at most n runes. Otherwise it
// returns the first n runes of s plus "…", so the result has n+1 runes, is
// valid UTF-8 when s is, and starts with a prefix of s.
func TestPropTruncate(t *testing.T) {
	alphabet := []rune("ab 亡灵🕯️[E1]\n")
	f := func(seed int64, n8 uint8) bool {
		r := rand.New(rand.NewSource(seed))
		b := make([]rune, r.Intn(400))
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		s, n := string(b), int(n8)
		out := truncate(s, n)
		if len(b) <= n {
			return out == s
		}
		return utf8.RuneCountInString(out) == n+1 && strings.HasSuffix(out, "…") &&
			strings.HasPrefix(s, strings.TrimSuffix(out, "…")) && utf8.ValidString(out)
	}
	t.Log("testing/quick seed=601 cases=5000")
	if err := quick.Check(f, &quick.Config{MaxCount: 5000, Rand: rand.New(rand.NewSource(601))}); err != nil {
		t.Fatal(err)
	}
}
