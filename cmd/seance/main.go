// Command seance summons dead Pods and prints how they died.
//
// Usage: seance [-n namespace] [-kubeconfig path] [-pod name] [-tail 200] [-fixtures]
//
// The command is read-only: it only lists and gets Pods, reads their logs and
// lists Events. With -fixtures it reads the embedded recordings and never
// touches a cluster. With SEANCE_RECORD=1 it also writes each collected Pod to
// a local fixture file (SEANCE_FIXTURES_DIR, default "fixtures").
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/k8s"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	fs := flag.NewFlagSet("seance", flag.ExitOnError)
	namespace := fs.String("n", "default", "namespace to search")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
	pod := fs.String("pod", "", "summon only this pod")
	tail := fs.Int("tail", 200, "log lines kept per container")
	useFixtures := fs.Bool("fixtures", false, "offline: summon the recorded fixture spirits, no cluster access")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: seance [-n namespace] [-kubeconfig path] [-pod name] [-tail 200] [-fixtures]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:]) // ExitOnError: bad flags exit with status 2, -h with 0.

	var err error
	if *useFixtures {
		err = runFixtures(context.Background(), *pod, *tail, os.Stdout)
	} else {
		err = run(context.Background(), *namespace, *kubeconfig, *pod, *tail, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "seance:", err)
		os.Exit(1)
	}
}

// run loads the kubeconfig, builds a clientset and summons the spirits.
func run(ctx context.Context, namespace, kubeconfig, pod string, tail int, out io.Writer) error {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig // empty keeps the default rules ($KUBECONFIG, ~/.kube/config)
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}
	return summon(ctx, cs, namespace, pod, tail, recorderFromEnv(os.Stderr), out)
}

// runFixtures prints the embedded recordings. It never loads a kubeconfig.
func runFixtures(ctx context.Context, pod string, tail int, out io.Writer) error {
	all, err := fixtures.Load()
	if err != nil {
		return fmt.Errorf("load fixtures: %w", err)
	}
	var spirits []*autopsy.PodAutopsy
	for _, a := range all {
		if pod == "" || a.Name == pod {
			spirits = append(spirits, a)
		}
	}
	if len(spirits) == 0 {
		if pod != "" {
			fmt.Fprintf(out, "no restless spirit fixtures/%s\n", pod)
		} else {
			fmt.Fprintf(out, "no restless spirits in fixtures\n")
		}
		return nil
	}
	for _, a := range spirits {
		if err := printAutopsy(ctx, a, tail, out); err != nil {
			return err
		}
	}
	return nil
}

// summon lists the spirits in namespace (or only pod) and prints each autopsy.
// A non-nil rec also records each collected autopsy to a fixture file.
func summon(ctx context.Context, cs kubernetes.Interface, namespace, pod string, tail int, rec *recorder, out io.Writer) error {
	spirits, err := k8s.ListSpirits(ctx, cs, namespace)
	if err != nil {
		return err
	}
	if pod != "" {
		spirits = filterPod(spirits, pod)
	}
	if len(spirits) == 0 {
		if pod != "" {
			fmt.Fprintf(out, "no restless spirit %s/%s\n", namespace, pod)
		} else {
			fmt.Fprintf(out, "no restless spirits in %s\n", namespace)
		}
		return nil
	}

	for _, s := range spirits {
		if err := printSpirit(ctx, cs, s.Namespace, s.Name, tail, rec, out); err != nil {
			return err
		}
	}
	return nil
}

// filterPod keeps only the summary whose name equals pod.
func filterPod(spirits []autopsy.SpiritSummary, pod string) []autopsy.SpiritSummary {
	var out []autopsy.SpiritSummary
	for _, s := range spirits {
		if s.Name == pod {
			out = append(out, s)
		}
	}
	return out
}

// printSpirit collects one Pod from the cluster, records it if rec is set, then prints it.
func printSpirit(ctx context.Context, cs kubernetes.Interface, namespace, name string, tail int, rec *recorder, out io.Writer) error {
	a, err := k8s.CollectAutopsy(ctx, cs, namespace, name)
	if err != nil {
		return err
	}
	if err := rec.record(a); err != nil {
		return err
	}
	return printAutopsy(ctx, a, tail, out)
}

// printAutopsy prints the header, the evidence list and the ghost's opening line for one PodAutopsy.
// It is shared by the cluster and fixtures paths.
func printAutopsy(ctx context.Context, a *autopsy.PodAutopsy, tail int, out io.Writer) error {
	c := autopsy.Classify(a)
	ev, err := autopsy.BuildEvidence(a, autopsy.EvidenceOptions{TailLines: tail})
	if err != nil {
		return fmt.Errorf("build evidence for %s/%s: %w", a.Namespace, a.Name, err)
	}

	exit := "-"
	if c.LastExitCode != nil {
		exit = fmt.Sprint(*c.LastExitCode)
	}
	restarts := int32(0)
	for _, ct := range a.Containers {
		restarts += ct.RestartCount
	}

	header := fmt.Sprintf("☠ %s/%s  cause=%s", a.Namespace, a.Name, c.Cause)
	if len(c.Secondary) > 0 {
		header += fmt.Sprintf(" secondary=%s", joinCauses(c.Secondary))
	}
	fmt.Fprintf(out, "%s exit=%s restarts=%d\n", header, exit, restarts)
	for _, e := range ev {
		fmt.Fprintf(out, "  [%s] (%s) %s\n", e.ID, e.Kind, e.Text)
	}

	fmt.Fprint(out, "  👻 ")
	_, err = llm.MockProvider{}.Stream(ctx, llm.ChatRequest{
		Messages: []llm.Message{{Role: "user", Content: "opening"}},
		Cause:    c.Cause,
		PodName:  a.Name,
		Evidence: ev,
	}, func(s string) error {
		_, werr := fmt.Fprint(out, s)
		return werr
	})
	if err != nil {
		return fmt.Errorf("stream opening for %s/%s: %w", a.Namespace, a.Name, err)
	}
	fmt.Fprint(out, "\n\n")
	return nil
}

func joinCauses(causes []autopsy.CauseOfDeath) string {
	parts := make([]string, 0, len(causes))
	for _, c := range causes {
		parts = append(parts, string(c))
	}
	return strings.Join(parts, ",")
}
