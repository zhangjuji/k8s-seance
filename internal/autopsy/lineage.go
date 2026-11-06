package autopsy

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Lineage verdicts (Requirement 16.3). Verdict picks them in this order.
const (
	// VerdictInstance: a sibling of the same revision is alive, so the problem
	// belongs to this instance (its node, its volume, its luck).
	VerdictInstance = "instance"
	// VerdictRegression: another revision has living Pods and none of this
	// revision is alive, so the release broke it.
	VerdictRegression = "regression"
	// VerdictFamily: every member of the family is dead, so look at what they
	// share: dependencies and configuration.
	VerdictFamily = "family"
	// VerdictAlone: the Pod has no siblings and no other revision to compare with.
	VerdictAlone = "alone"
)

// Version ages relative to the spirit's revision.
const (
	AgeOlder = "older" // a former life (前世)
	AgeNewer = "newer" // a later life (转世)
)

// maxLineageDiffs is the number of differences listed per revision in the evidence.
const maxLineageDiffs = 8

// maxLineagePods is the number of Pods named per revision in the evidence.
const maxLineagePods = 5

// Lineage is the family of a Pod: the Pods of the same controller, grouped by
// revision (pod-template-hash or controller-revision-hash).
type Lineage struct {
	// OwnerKind is Deployment, StatefulSet or DaemonSet.
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
	// Inferred is true when OwnerName was derived from the ReplicaSet name by
	// removing "-<pod-template-hash>". The Deployment itself was not read.
	Inferred      bool   `json:"inferred,omitempty"`
	ViaReplicaSet string `json:"viaReplicaSet,omitempty"`
	// HashLabel is pod-template-hash or controller-revision-hash.
	HashLabel string `json:"hashLabel"`
	// Revision is the spirit's value of HashLabel.
	Revision string    `json:"revision"`
	Self     Kin       `json:"self"`
	Siblings []Kin     `json:"siblings,omitempty"`
	Versions []Version `json:"versions,omitempty"` // other revisions, oldest first
}

// Kin is one Pod of the family.
type Kin struct {
	Name      string    `json:"name"`
	Node      string    `json:"node,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Alive     bool      `json:"alive"`
	State     string    `json:"state,omitempty"` // e.g. "running", "CrashLoopBackOff", "OOMKilled"
	Restarts  int32     `json:"restarts"`
	CreatedAt time.Time `json:"createdAt"`
}

// Version is one other revision of the family.
type Version struct {
	Revision string `json:"revision"`
	// Age is AgeOlder or AgeNewer, from the oldest Pod creation time of each revision.
	Age   string     `json:"age"`
	Pods  []Kin      `json:"pods"`
	Diffs []SpecDiff `json:"diffs,omitempty"`
}

// SpecDiff is one difference between the Pod template of a revision and the
// spirit's. From is the other revision's value, To is the spirit's. When Hidden
// is true the values are secret and both are empty: only the change is shown.
type SpecDiff struct {
	Field  string `json:"field"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// Alive is the number of living Pods of the revision.
func (v Version) Alive() int { return countAlive(v.Pods) }

// AliveSiblings is the number of living siblings. The spirit itself never counts.
func (l *Lineage) AliveSiblings() int {
	if l == nil {
		return 0
	}
	return countAlive(l.Siblings)
}

// Verdict judges the family: instance > regression > family; alone when there
// is nobody to compare with.
func (l *Lineage) Verdict() string {
	if l == nil {
		return ""
	}
	if l.AliveSiblings() > 0 {
		return VerdictInstance
	}
	for _, v := range l.Versions {
		if v.Alive() > 0 {
			return VerdictRegression
		}
	}
	if len(l.Siblings) == 0 && len(l.Versions) == 0 {
		return VerdictAlone
	}
	return VerdictFamily
}

// LivingFormerLife returns the newest older revision that still has living Pods,
// when the verdict is regression. It is the revision a rollback goes back to.
func (l *Lineage) LivingFormerLife() *Version {
	if l.Verdict() != VerdictRegression {
		return nil
	}
	var out *Version
	for i := range l.Versions {
		if v := &l.Versions[i]; v.Age == AgeOlder && v.Alive() > 0 {
			out = v
		}
	}
	return out
}

// Owner is "Deployment/name".
func (l *Lineage) Owner() string { return l.OwnerKind + "/" + l.OwnerName }

func countAlive(pods []Kin) int {
	n := 0
	for _, p := range pods {
		if p.Alive {
			n++
		}
	}
	return n
}

// verdictMeaning explains each verdict in the evidence text.
var verdictMeaning = map[string]string{
	VerdictInstance:   "a sibling of the same revision is alive, so this is an instance problem: compare the nodes",
	VerdictRegression: "another revision has living Pods and none of this revision is alive, so the release broke it",
	VerdictFamily:     "every member of the family is dead, so check the dependencies and configuration they share",
	VerdictAlone:      "no siblings and no other revisions to compare with",
}

// LineageTexts is the family evidence (kind "lineage"): one item for the family
// and the verdict, then one item per other revision with its living Pods and at
// most 8 differences. It returns nil for a nil lineage.
func LineageTexts(l *Lineage) []string {
	if l == nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "family %s", l.Owner())
	if l.Inferred && l.ViaReplicaSet != "" {
		fmt.Fprintf(&b, " (inferred from ReplicaSet %s)", l.ViaReplicaSet)
	}
	v := l.Verdict()
	fmt.Fprintf(&b, ": verdict=%s (%s); this revision %s=%s: alive %d/%d (this Pod %s on node %s, %s); siblings: %s",
		v, verdictMeaning[v], l.HashLabel, l.Revision,
		l.AliveSiblings(), len(l.Siblings)+1,
		l.Self.Name, orNone(l.Self.Node), kinState(l.Self), kinList(l.Siblings))
	out := []string{b.String()}
	for _, ver := range l.Versions {
		out = append(out, versionText(l, ver))
	}
	return out
}

func versionText(l *Lineage, v Version) string {
	life := "former life"
	if v.Age == AgeNewer {
		life = "later life"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "revision %s=%s (%s, %s): alive %d/%d; pods: %s; differs from this revision: ",
		l.HashLabel, v.Revision, v.Age, life, v.Alive(), len(v.Pods), kinList(v.Pods))
	if len(v.Diffs) == 0 {
		b.WriteString("nothing in the compared fields")
		return b.String()
	}
	parts := make([]string, 0, maxLineageDiffs+1)
	for i, d := range v.Diffs {
		if i == maxLineageDiffs {
			parts = append(parts, fmt.Sprintf("(+%d more)", len(v.Diffs)-maxLineageDiffs))
			break
		}
		parts = append(parts, DiffText(d))
	}
	b.WriteString(strings.Join(parts, "; "))
	return b.String()
}

// DiffText renders one difference as "field: from -> to", or
// "field: changed (value hidden)" for a secret value.
func DiffText(d SpecDiff) string {
	if d.Hidden {
		return d.Field + ": changed (value hidden)"
	}
	return d.Field + ": " + orNone(d.From) + " -> " + orNone(d.To)
}

func kinList(pods []Kin) string {
	if len(pods) == 0 {
		return "none"
	}
	parts := make([]string, 0, maxLineagePods+1)
	for i, p := range pods {
		if i == maxLineagePods {
			parts = append(parts, fmt.Sprintf("+%d more", len(pods)-maxLineagePods))
			break
		}
		parts = append(parts, fmt.Sprintf("%s on node %s (%s)", p.Name, orNone(p.Node), kinState(p)))
	}
	return strings.Join(parts, ", ")
}

func kinState(p Kin) string {
	if p.Alive {
		return "alive"
	}
	if p.State != "" {
		return "dead " + p.State
	}
	return "dead"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// secretName matches environment variable names whose values are never shown.
var secretName = regexp.MustCompile(`(?i)pass(word|wd)?|secret|token|api[_-]?key|private[_-]?key|access[_-]?key|credential|auth|cert|cookie|session`)

// SecretName reports whether an environment variable name looks like it holds a secret.
func SecretName(name string) bool { return secretName.MatchString(name) }

// EnvValue returns how an environment variable value may be shown. The value
// is hidden when the name looks like a secret or when the evidence redaction
// would change it.
func EnvValue(name, value string) (shown string, hidden bool) {
	if SecretName(name) {
		return "", true
	}
	if Redact(value) != value || Redact(name+"="+value) != name+"="+value {
		return "", true
	}
	return value, false
}
