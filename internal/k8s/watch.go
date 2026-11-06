package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Default reconnect backoff of WatchPods (design: 1s first, at most 30s).
const (
	DefaultWatchMinBackoff = time.Second
	DefaultWatchMaxBackoff = 30 * time.Second
)

// errWatchClosed is reported when the API server ends a watch.
var errWatchClosed = errors.New("pod watch closed by the server")

// WatchOptions tunes WatchPods. The zero value uses the defaults.
type WatchOptions struct {
	// MinBackoff is the first reconnect delay. Zero means DefaultWatchMinBackoff.
	MinBackoff time.Duration
	// MaxBackoff caps the reconnect delay, which doubles after each failure.
	// Zero means DefaultWatchMaxBackoff.
	MaxBackoff time.Duration
	// OnTrouble, if set, is called from the watch goroutine each time the watch
	// cannot be opened or breaks. WatchPods keeps reconnecting on its own.
	OnTrouble func(error)
}

// WatchPods watches the Pods in namespace with the read-only Pods(ns).Watch call
// and sends a value on the returned channel whenever a Pod may have changed.
// The events themselves are not passed on: callers re-list to see what changed.
//
// Signals coalesce: the channel has room for one, and a signal that finds it
// full is dropped, because the pending one already says "something changed".
//
// When the watch cannot be opened, ends, or reports an error event, WatchPods
// waits (MinBackoff, doubling up to MaxBackoff) and opens a new watch. After a
// reconnect it sends one signal, because changes may have been missed while it
// was away. The backoff starts again from MinBackoff once a watch has delivered
// an event. The channel is closed when ctx is done. An empty namespace means
// "default".
func WatchPods(ctx context.Context, cs kubernetes.Interface, namespace string, opts WatchOptions) <-chan struct{} {
	if namespace == "" {
		namespace = defaultNamespace
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = DefaultWatchMinBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultWatchMaxBackoff
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = opts.MinBackoff
	}

	out := make(chan struct{}, 1)
	signal := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	trouble := func(err error) {
		if opts.OnTrouble != nil && ctx.Err() == nil {
			opts.OnTrouble(err)
		}
	}

	go func() {
		defer close(out)
		delay := opts.MinBackoff
		reconnecting := false
		for ctx.Err() == nil {
			w, err := cs.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{})
			if err != nil {
				trouble(fmt.Errorf("watch pods in namespace %q: %w", namespace, err))
			} else {
				if reconnecting {
					signal()
				}
				gotEvent, err := drain(ctx, w, signal)
				if ctx.Err() != nil {
					return
				}
				if gotEvent {
					delay = opts.MinBackoff
				}
				trouble(err)
			}
			reconnecting = true
			if !sleep(ctx, delay) {
				return
			}
			delay = nextBackoff(delay, opts.MaxBackoff)
		}
	}()
	return out
}

// drain reads one watch until it ends, signalling once per event. It reports
// whether any event arrived and why the watch ended. ctx ending stops the watch.
func drain(ctx context.Context, w watch.Interface, signal func()) (bool, error) {
	defer w.Stop()
	got := false
	for {
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return got, errWatchClosed
			}
			if ev.Type == watch.Error {
				return got, fmt.Errorf("pod watch error: %w", apierrors.FromObject(ev.Object))
			}
			got = true
			signal()
		}
	}
}

// nextBackoff doubles d, capped at limit.
func nextBackoff(d, limit time.Duration) time.Duration {
	d *= 2
	if d > limit {
		return limit
	}
	return d
}

// sleep waits for d or until ctx is done. It reports whether the full wait passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
