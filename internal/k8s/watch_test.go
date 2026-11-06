package k8s

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// watchScript hands out one scripted result per Watch call: a FakeWatcher the
// test drives, or an error. Calls beyond the script block on a watcher that never
// sends, so the loop stays quiet until the test ends.
type watchScript struct {
	mu      sync.Mutex
	results []error // nil entry: open a watcher
	opened  chan *watch.FakeWatcher
	calls   int
}

func newWatchScript(results ...error) *watchScript {
	return &watchScript{results: results, opened: make(chan *watch.FakeWatcher, 16)}
}

func (s *watchScript) react(action k8stesting.Action) (bool, watch.Interface, error) {
	s.mu.Lock()
	i := s.calls
	s.calls++
	s.mu.Unlock()
	if i < len(s.results) && s.results[i] != nil {
		return true, nil, s.results[i]
	}
	w := watch.NewFakeWithChanSize(4, false)
	s.opened <- w
	return true, w, nil
}

func (s *watchScript) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func fastWatch(onTrouble func(error)) WatchOptions {
	return WatchOptions{MinBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond, OnTrouble: onTrouble}
}

func waitWatcher(t *testing.T, s *watchScript) *watch.FakeWatcher {
	t.Helper()
	select {
	case w := <-s.opened:
		return w
	case <-time.After(2 * time.Second):
		t.Fatal("no watch was opened")
		return nil
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("%s: channel closed", what)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: no signal", what)
	}
}

func noSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s: unexpected signal", what)
	case <-time.After(30 * time.Millisecond):
	}
}

func pod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: name}}
}

func TestWatchPodsSignalsAndReconnects(t *testing.T) {
	script := newWatchScript()
	cs := fake.NewClientset()
	cs.PrependWatchReactor("pods", script.react)

	var troubles []error
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchPods(ctx, cs, "ns1", fastWatch(func(err error) {
		mu.Lock()
		troubles = append(troubles, err)
		mu.Unlock()
	}))

	w := waitWatcher(t, script)
	noSignal(t, ch, "first watch open")

	w.Add(pod("a"))
	waitSignal(t, ch, "added event")
	w.Modify(pod("a"))
	waitSignal(t, ch, "modified event")

	// The server ends the watch. WatchPods reconnects and sends one catch-up signal.
	w.Stop()
	w2 := waitWatcher(t, script)
	waitSignal(t, ch, "catch-up after reconnect")
	noSignal(t, ch, "after catch-up")

	w2.Delete(pod("a"))
	waitSignal(t, ch, "deleted event on the new watch")

	mu.Lock()
	if len(troubles) != 1 || !errors.Is(troubles[0], errWatchClosed) {
		t.Errorf("troubles = %v, want one errWatchClosed", troubles)
	}
	mu.Unlock()

	cancel()
	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}

	for _, a := range cs.Actions() {
		if a.GetVerb() != "watch" || a.GetResource().Resource != "pods" || a.GetNamespace() != "ns1" {
			t.Errorf("unexpected action %s %s in %q", a.GetVerb(), a.GetResource().Resource, a.GetNamespace())
		}
	}
}

func TestWatchPodsRetriesFailedOpenThenCatchesUp(t *testing.T) {
	forbidden := errors.New("forbidden")
	script := newWatchScript(forbidden, forbidden)
	cs := fake.NewClientset()
	cs.PrependWatchReactor("pods", script.react)

	troubled := make(chan error, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchPods(ctx, cs, "ns1", fastWatch(func(err error) { troubled <- err }))

	waitWatcher(t, script)
	waitSignal(t, ch, "catch-up after the watch finally opened")
	if got := script.callCount(); got != 3 {
		t.Errorf("Watch calls = %d, want 3", got)
	}
	if n := len(troubled); n != 2 {
		t.Errorf("trouble reports = %d, want 2", n)
	}
	for i := 0; i < len(troubled); i++ {
		if err := <-troubled; !errors.Is(err, forbidden) {
			t.Errorf("trouble = %v, want the open error", err)
		}
	}
}

func TestWatchPodsErrorEventReconnects(t *testing.T) {
	script := newWatchScript()
	cs := fake.NewClientset()
	cs.PrependWatchReactor("pods", script.react)
	troubled := make(chan error, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchPods(ctx, cs, "", fastWatch(func(err error) { troubled <- err }))

	w := waitWatcher(t, script)
	w.Error(&metav1.Status{Status: metav1.StatusFailure, Message: "too old resource version", Code: 410})
	waitWatcher(t, script)
	waitSignal(t, ch, "catch-up after error event")
	select {
	case err := <-troubled:
		if err == nil {
			t.Error("nil trouble")
		}
	case <-time.After(time.Second):
		t.Error("error event was not reported")
	}
	for _, a := range cs.Actions() {
		if a.GetNamespace() != "default" {
			t.Errorf("namespace = %q, want default", a.GetNamespace())
		}
	}
}

func TestWatchPodsCoalescesSignals(t *testing.T) {
	script := newWatchScript()
	cs := fake.NewClientset()
	cs.PrependWatchReactor("pods", script.react)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := WatchPods(ctx, cs, "ns1", fastWatch(nil))

	w := waitWatcher(t, script)
	for _, name := range []string{"a", "b", "c"} {
		w.Add(pod(name))
	}
	time.Sleep(30 * time.Millisecond)
	waitSignal(t, ch, "coalesced")
	noSignal(t, ch, "second read")
}

func TestNextBackoff(t *testing.T) {
	d := DefaultWatchMinBackoff
	var got []time.Duration
	for i := 0; i < 7; i++ {
		got = append(got, d)
		d = nextBackoff(d, DefaultWatchMaxBackoff)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("backoff[%d] = %v, want %v", i, got[i], want[i]*time.Second)
		}
	}
}
