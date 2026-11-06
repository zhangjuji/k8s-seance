// Command seance serves the spirit list in a local Web UI, or prints autopsies
// to stdout with the summon subcommand.
//
// Usage:
//
//	seance [-addr localhost:6660] [-n namespace] [-kubeconfig path] [-tail 200] [-graveyard path] [-fixtures] [-replay | -mock]
//	seance summon [-n namespace] [-kubeconfig path] [-pod name] [-tail 200] [-fixtures] [-replay | -mock]
//
// The command is read-only: it only lists and gets Pods, reads their logs and
// lists Events. With -fixtures it reads the embedded recordings, never loads a
// kubeconfig and never touches a cluster. If the cluster cannot be set up at
// startup, the web UI still starts and shows the reason with a link to fixture mode.
// With SEANCE_RECORD=1 the summon subcommand also writes each collected Pod to
// a local fixture file (SEANCE_FIXTURES_DIR, default "fixtures"). The spirit's
// opening line comes from the LLM provider: -replay uses the recorded replies,
// -mock uses the template, and otherwise SEANCE_LLM_MODE or the config decide.
// With SEANCE_RECORD=1 and an HTTP provider, the replies are recorded to
// fixtures/replay. Turns already recorded there are replayed without an API call,
// so only missing turns reach the API.
// Each generated postmortem also becomes a tombstone in the local graveyard file
// (-graveyard, default ~/.k8s-seance/graveyard.json; an empty value turns it off).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zhangjuji/k8s-seance/fixtures"
	"github.com/zhangjuji/k8s-seance/internal/autopsy"
	"github.com/zhangjuji/k8s-seance/internal/graveyard"
	"github.com/zhangjuji/k8s-seance/internal/k8s"
	"github.com/zhangjuji/k8s-seance/internal/llm"
	"github.com/zhangjuji/k8s-seance/internal/web"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// shutdownTimeout bounds how long in-flight requests get after SIGINT or SIGTERM.
const shutdownTimeout = 5 * time.Second

// errFixturesOnly is the reason the cluster is unavailable in --fixtures mode.
var errFixturesOnly = errors.New("fixtures-only mode (--fixtures) does not connect to a cluster")

func main() {
	args := os.Args[1:]
	var err error
	if len(args) > 0 && args[0] == "summon" {
		err = runSummon(args[1:])
	} else {
		err = runServe(args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "seance:", err)
		os.Exit(1)
	}
}

// runServe starts the Web UI. It does not load a kubeconfig in --fixtures mode.
// Otherwise a failed clientset build is logged once and the UI starts without a cluster.
func runServe(args []string) error {
	fs := flag.NewFlagSet("seance", flag.ExitOnError)
	addr := fs.String("addr", "localhost:6660", "listen address")
	namespace := fs.String("n", "default", "default namespace")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
	tail := fs.Int("tail", 200, "log lines kept per container")
	useFixtures := fs.Bool("fixtures", false, "offline: show the recorded fixture spirits, no cluster access")
	useReplay := fs.Bool("replay", false, "LLM replies from fixtures/replay (falls back to mock per turn)")
	useMock := fs.Bool("mock", false, "LLM replies from the built-in template, no API calls")
	graveyardPath := fs.String("graveyard", defaultGraveyardPath(), "tombstone file for generated postmortems (empty: graveyard off)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: seance [-addr localhost:6660] [-n namespace] [-kubeconfig path] [-tail 200] [-graveyard path] [-fixtures] [-replay | -mock]\n       seance summon [flags]   print autopsies to stdout\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // ExitOnError: bad flags exit with status 2, -h with 0.

	mode, err := selectMode(*useReplay, *useMock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seance:", err)
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "seance: ", 0)
	provider := buildProvider(mode, os.Stderr)
	cluster, clusterErr := openCluster(*useFixtures, *kubeconfig, *tail, logger)
	cfg, _ := llm.LoadConfig() // buildProvider already reported any config problem
	grave := openGraveyard(*graveyardPath, logger)

	srv, err := web.New(web.Options{
		Cluster:          cluster,
		ClusterErr:       clusterErr,
		DefaultNamespace: *namespace,
		FixturesOnly:     *useFixtures,
		Provider:         provider,
		Limits:           llm.LimitsFromEnv(),
		Model:            cfg.Model,
		Logger:           logger,
		Graveyard:        grave,
	})
	if err != nil {
		return err
	}
	return serveHTTP(*addr, srv.Handler(), logger)
}

// defaultGraveyardPath is ~/.k8s-seance/graveyard.json, or "" when the home
// directory is unknown.
func defaultGraveyardPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".k8s-seance", "graveyard.json")
}

// openGraveyard returns the store for path, or nil when path is empty.
// The file is only created when the first tombstone is written.
func openGraveyard(path string, logger *log.Logger) *graveyard.Store {
	path = strings.TrimSpace(path)
	if path == "" {
		logger.Printf("graveyard off: no tombstones will be written")
		return nil
	}
	st, err := graveyard.Open(path)
	if err != nil {
		logger.Printf("warning: graveyard off: %v", err)
		return nil
	}
	logger.Printf("graveyard: %s", st.Path())
	return st
}

// openCluster returns the cluster source, or nil with the reason it is unavailable.
// It only reads the kubeconfig and builds a client. It makes no API request.
func openCluster(fixturesOnly bool, kubeconfig string, tail int, logger *log.Logger) (web.Source, error) {
	if fixturesOnly {
		return nil, errFixturesOnly
	}
	cs, err := newClientset(kubeconfig)
	if err != nil {
		logger.Printf("warning: no cluster connection, the UI will show the reason: %v", err)
		return nil, err
	}
	return web.ClusterSource{Client: cs, Tail: tail}, nil
}

// serveHTTP listens on addr and serves handler until SIGINT or SIGTERM, then shuts down gracefully.
func serveHTTP(addr string, handler http.Handler, logger *log.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	logger.Printf("listening on http://%s (Ctrl+C to stop)", ln.Addr())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	logger.Println("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runSummon is the previous text output: it prints each autopsy and its opening line.
func runSummon(args []string) error {
	fs := flag.NewFlagSet("seance summon", flag.ExitOnError)
	namespace := fs.String("n", "default", "namespace to search")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
	pod := fs.String("pod", "", "summon only this pod")
	tail := fs.Int("tail", 200, "log lines kept per container")
	useFixtures := fs.Bool("fixtures", false, "offline: summon the recorded fixture spirits, no cluster access")
	useReplay := fs.Bool("replay", false, "LLM replies from fixtures/replay (falls back to mock per turn)")
	useMock := fs.Bool("mock", false, "LLM replies from the built-in template, no API calls")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: seance summon [-n namespace] [-kubeconfig path] [-pod name] [-tail 200] [-fixtures] [-replay | -mock]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	mode, err := selectMode(*useReplay, *useMock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seance:", err)
		os.Exit(2)
	}
	provider := buildProvider(mode, os.Stderr)

	if *useFixtures {
		return runFixtures(context.Background(), *pod, *tail, provider, os.Stdout)
	}
	return run(context.Background(), *namespace, *kubeconfig, *pod, *tail, provider, os.Stdout)
}

// selectMode returns the LLM mode named by the flags. Without a flag it returns
// SEANCE_LLM_MODE, which may be empty. Passing both flags is an error.
func selectMode(replay, mock bool) (string, error) {
	switch {
	case replay && mock:
		return "", errors.New("-replay and -mock are mutually exclusive")
	case replay:
		return llm.ModeReplay, nil
	case mock:
		return llm.ModeMock, nil
	}
	return strings.ToLower(strings.TrimSpace(os.Getenv("SEANCE_LLM_MODE"))), nil
}

// replayDir is where SEANCE_RECORD=1 writes LLM replies, relative to the working directory.
const replayDir = "fixtures/replay"

// buildProvider builds the LLM provider for mode. A notice goes to errOut when
// HTTP was requested but the config is incomplete. The notice never includes the key.
func buildProvider(mode string, errOut io.Writer) llm.Provider {
	cfg, err := llm.LoadConfig()
	if err != nil {
		fmt.Fprintf(errOut, "seance: %v; using mock\n", err)
		cfg = llm.Config{}
	}
	if mode == llm.ModeHTTP && !cfg.Complete() {
		fmt.Fprintln(errOut, "seance: LLM config incomplete (base URL, API key or model missing); falling back to mock")
	}
	record := os.Getenv("SEANCE_RECORD") == "1"
	return llm.NewProvider(cfg, mode, record, fixtures.ReplayFS(), replayDir)
}

// run loads the kubeconfig, builds a clientset and summons the spirits.
func run(ctx context.Context, namespace, kubeconfig, pod string, tail int, provider llm.Provider, out io.Writer) error {
	cs, err := newClientset(kubeconfig)
	if err != nil {
		return err
	}
	return summon(ctx, cs, namespace, pod, tail, recorderFromEnv(os.Stderr), provider, out)
}

// newClientset loads the kubeconfig and builds a client. It makes no request to the API server.
// An empty kubeconfig means the default rules: $KUBECONFIG, then ~/.kube/config.
func newClientset(kubeconfig string) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return cs, nil
}

// runFixtures prints the embedded recordings. It never loads a kubeconfig.
func runFixtures(ctx context.Context, pod string, tail int, provider llm.Provider, out io.Writer) error {
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
		if err := printAutopsy(ctx, a, tail, provider, out); err != nil {
			return err
		}
	}
	return nil
}

// summon lists the spirits in namespace (or only pod) and prints each autopsy.
// A non-nil rec also records each collected autopsy to a fixture file.
func summon(ctx context.Context, cs kubernetes.Interface, namespace, pod string, tail int, rec *recorder, provider llm.Provider, out io.Writer) error {
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
		if err := printSpirit(ctx, cs, s.Namespace, s.Name, tail, rec, provider, out); err != nil {
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
func printSpirit(ctx context.Context, cs kubernetes.Interface, namespace, name string, tail int, rec *recorder, provider llm.Provider, out io.Writer) error {
	a, err := k8s.CollectAutopsy(ctx, cs, namespace, name)
	if err != nil {
		return err
	}
	if err := rec.record(a); err != nil {
		return err
	}
	return printAutopsy(ctx, a, tail, provider, out)
}

// printAutopsy prints the header, the evidence list and the ghost's opening line for one PodAutopsy.
// It is shared by the cluster and fixtures paths.
func printAutopsy(ctx context.Context, a *autopsy.PodAutopsy, tail int, provider llm.Provider, out io.Writer) error {
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
	_, err = provider.Stream(ctx, llm.ChatRequest{
		Messages:  []llm.Message{{Role: "user", Content: "opening"}},
		Cause:     c.Cause,
		PodName:   a.Name,
		Evidence:  ev,
		FixtureID: a.Name,
		TurnIndex: 0,
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
