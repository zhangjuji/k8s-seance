// Package web serves the local Web UI: net/http routes, html/template pages and
// the embedded htmx assets. Pages and the API read spirits only through Source.
package web

import (
	"context"
	"errors"
	"time"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/k8s"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

// ErrNotFound is returned by Source.Autopsy when no spirit has the requested name.
var ErrNotFound = errors.New("spirit not found")

// Source is where spirit lists and autopsies come from: the live cluster or the bundled fixtures.
type Source interface {
	// Name is "cluster" or "fixtures".
	Name() string
	// List returns one summary per spirit. Each summary has Cause, RestartCount and LastDeath filled.
	List(ctx context.Context, namespace string) ([]autopsy.SpiritSummary, error)
	// Autopsy returns the collected snapshot of one spirit, or ErrNotFound.
	Autopsy(ctx context.Context, namespace, name string) (*autopsy.PodAutopsy, error)
}

// ClusterSource reads spirits from a live cluster through the read-only k8s collector.
type ClusterSource struct {
	Client kubernetes.Interface
	// Tail keeps at most this many trailing log lines per container. Zero keeps
	// everything the collector returns (at most 200 lines).
	Tail int
	// EventTTL is the kube-apiserver event TTL used for the memory countdown.
	// Zero means autopsy.DefaultEventTTL.
	EventTTL time.Duration
	// Now is the clock the countdown starts from. Nil means time.Now.
	Now func() time.Time
}

// Name implements Source.
func (ClusterSource) Name() string { return "cluster" }

// List implements Source. It makes one Pods list and one namespace-wide Events list,
// whatever the number of spirits. Cause comes from autopsy.Classify on the list-only
// autopsies, so no Get or log read is made here. Memory counts from now with
// EventTTL (Requirement 14). Log tails are not read, and
// Classify does not use them. A Pod does not appear in the list if it is gone from the
// cluster by the time the list is read.
func (c ClusterSource) List(ctx context.Context, namespace string) ([]autopsy.SpiritSummary, error) {
	listings, err := k8s.ListSpiritAutopsies(ctx, c.Client, namespace)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	asOf := now()
	out := make([]autopsy.SpiritSummary, 0, len(listings))
	for _, l := range listings {
		s := l.Summary
		s.Cause = autopsy.Classify(l.Autopsy).Cause
		m := autopsy.Remember(l.Autopsy, c.EventTTL, asOf)
		s.Memory = &m
		out = append(out, s)
	}
	return out, nil
}

// Autopsy implements Source. A missing Pod returns ErrNotFound.
func (c ClusterSource) Autopsy(ctx context.Context, namespace, name string) (*autopsy.PodAutopsy, error) {
	a, err := k8s.CollectAutopsy(ctx, c.Client, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for i := range a.Containers {
		ct := &a.Containers[i]
		ct.LogTail = keepLastLines(ct.LogTail, c.Tail)
		ct.PreviousLogTail = keepLastLines(ct.PreviousLogTail, c.Tail)
	}
	return a, nil
}

// keepLastLines returns the last n lines. A nil slice stays nil, which means "no record".
func keepLastLines(lines []string, n int) []string {
	if n <= 0 || len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// FixtureSource serves the recorded spirits from the fixtures package. It never
// touches a cluster. List ignores the requested namespace, because the fixtures
// come from one demo namespace and the page says so. Autopsy also ignores the
// namespace and finds the spirit by name.
// The memory countdown counts from each fixture's collectedAt, the moment it
// was recorded, so the recorded spirits are not all forgotten (Requirement 14.5).
type FixtureSource struct {
	load func() ([]*autopsy.PodAutopsy, error)
	// EventTTL is the event TTL for the countdown. Zero means autopsy.DefaultEventTTL.
	EventTTL time.Duration
}

// NewFixtureSource returns the source backed by the embedded fixtures.
func NewFixtureSource() *FixtureSource {
	return &FixtureSource{load: fixtures.Load}
}

// Name implements Source.
func (*FixtureSource) Name() string { return "fixtures" }

// List implements Source. Each summary is built the same way as the cluster
// summaries: Cause from autopsy.Classify, RestartCount summed over containers,
// and LastDeath as the latest FinishedAt of any container state or last state.
func (f *FixtureSource) List(_ context.Context, _ string) ([]autopsy.SpiritSummary, error) {
	all, err := f.load()
	if err != nil {
		return nil, err
	}
	out := make([]autopsy.SpiritSummary, 0, len(all))
	for _, a := range all {
		s := summarizeAutopsy(a)
		m := autopsy.Remember(a, f.EventTTL, memoryAsOf(a, time.Now()))
		s.Memory = &m
		out = append(out, s)
	}
	return out, nil
}

// Autopsy implements Source. The namespace argument is ignored.
func (f *FixtureSource) Autopsy(_ context.Context, _, name string) (*autopsy.PodAutopsy, error) {
	all, err := f.load()
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.Name == name {
			return a, nil
		}
	}
	return nil, ErrNotFound
}

// summarizeAutopsy builds the list row for a collected Pod, using the same rules
// as k8s.ListSpirits for RestartCount and LastDeath.
func summarizeAutopsy(a *autopsy.PodAutopsy) autopsy.SpiritSummary {
	s := autopsy.SpiritSummary{
		Namespace: a.Namespace,
		Name:      a.Name,
		Cause:     autopsy.Classify(a).Cause,
		Family:    autopsy.FamilyOf(a),
	}
	var latest *time.Time
	for _, ct := range a.Containers {
		s.RestartCount += ct.RestartCount
		latest = laterTime(latest, ct.State.FinishedAt)
		if ct.LastState != nil {
			latest = laterTime(latest, ct.LastState.FinishedAt)
		}
	}
	s.LastDeath = latest
	return s
}

// laterTime returns the later of cur and t, where either may be nil.
func laterTime(cur, t *time.Time) *time.Time {
	if t == nil {
		return cur
	}
	if cur == nil || t.After(*cur) {
		v := *t
		return &v
	}
	return cur
}

// memoryAsOf is the moment a countdown for a starts from: the recording time
// for a fixture, so that sample mode does not show every spirit as forgotten,
// and now for a live Pod.
func memoryAsOf(a *autopsy.PodAutopsy, now time.Time) time.Time {
	if a != nil && a.Source == "fixture" && !a.CollectedAt.IsZero() {
		return a.CollectedAt
	}
	return now
}
