package postmortem

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

// logLabel matches the "container X current log: " or "previous log: " prefix of a log evidence text.
var logLabel = regexp.MustCompile(`^container \S+ (?:current|previous) log: `)

// errorLine matches log lines that look like a failure.
var errorLine = regexp.MustCompile(`(?i)error|fatal|panic|exception|killed|failed`)

// facts is the snapshot and evidence data the rule text uses. Each evidence
// pointer is nil when no item of that kind exists. Citations are built only from
// these pointers, so every [E#] written by the rules is in the evidence list.
type facts struct {
	cause    autopsy.CauseOfDeath
	exitCode string // "" when the classifier has no exit code
	restarts int32
	image    string // first container image, "" when none
	memLimit string // first container memory limit, "" when none

	first     *autopsy.Evidence // first evidence item of any kind
	status    *autopsy.Evidence // first status item
	limitSpec *autopsy.Evidence // first spec item with a resource limit
	imageSpec *autopsy.Evidence // first spec item with a container image
	warning   *autopsy.Evidence // first Warning event
	errLog    *autopsy.Evidence // first log line that looks like an error
}

// collectFacts reads the facts and the evidence anchors from in.
func collectFacts(in Input) facts {
	f := facts{cause: in.Class.Cause, restarts: restarts(in.Autopsy)}
	if in.Class.LastExitCode != nil {
		f.exitCode = fmt.Sprint(*in.Class.LastExitCode)
	}
	for _, c := range in.Autopsy.Containers {
		if f.image == "" {
			f.image = c.Image
		}
		if f.memLimit == "" {
			f.memLimit = c.Limits["memory"]
		}
	}

	if len(in.Evidence) > 0 {
		f.first = &in.Evidence[0]
	}
	for i := range in.Evidence {
		e := &in.Evidence[i]
		switch {
		case e.Kind == "status" && f.status == nil:
			f.status = e
		case e.Kind == "spec" && f.limitSpec == nil &&
			strings.Contains(e.Text, " limits: ") && !strings.Contains(e.Text, "limits: none"):
			f.limitSpec = e
		case e.Kind == "spec" && f.imageSpec == nil && strings.Contains(e.Text, " image: "):
			f.imageSpec = e
		case e.Kind == "event" && f.warning == nil && strings.Contains(e.Text, " Warning "):
			f.warning = e
		case e.Kind == "log" && f.errLog == nil && errorLine.MatchString(logLabel.ReplaceAllString(e.Text, "")):
			f.errLog = e
		}
	}
	return f
}

// detail returns the known exit code and restart count, for example
// " (exit code 137, 8 restarts)". It returns "" when neither is known.
func (f facts) detail() string {
	var parts []string
	if f.exitCode != "" {
		parts = append(parts, "exit code "+f.exitCode)
	}
	if f.restarts > 0 {
		parts = append(parts, fmt.Sprintf("%d restarts", f.restarts))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// withFallback returns the non-nil items in evs. If there are none, it returns the
// first evidence item, so a sentence still cites real evidence when any exists.
func withFallback(f facts, evs ...*autopsy.Evidence) []*autopsy.Evidence {
	var out []*autopsy.Evidence
	for _, e := range evs {
		if e != nil {
			out = append(out, e)
		}
	}
	if len(out) == 0 && f.first != nil {
		out = append(out, f.first)
	}
	return out
}

// refs renders the non-nil evidence IDs in the order given, without repeats, for example "[E1][E3]".
func refs(evs ...*autopsy.Evidence) string {
	var b strings.Builder
	seen := make(map[string]bool, len(evs))
	for _, e := range evs {
		if e == nil || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		b.WriteString("[" + e.ID + "]")
	}
	return b.String()
}

// sentence ends text with its citations and a period. The text must not end in a period.
func sentence(text string, evs ...*autopsy.Evidence) string {
	if r := refs(evs...); r != "" {
		return text + " " + r + "."
	}
	return text + "."
}

// bullet is a remediation line.
func bullet(text string, evs ...*autopsy.Evidence) string {
	return "- " + sentence(text, evs...)
}

// action is an action item line. It starts with the checkbox the postmortem test expects.
func action(text string, evs ...*autopsy.Evidence) string {
	return "- [ ] " + sentence(text, evs...)
}

// ruleSections returns the postmortem blocks for the cause without the LLM. It is
// deterministic: the same snapshot and evidence always give the same text.
func ruleSections(f facts) sections {
	switch f.cause {
	case autopsy.CauseOOMKilled:
		return oomRules(f)
	case autopsy.CauseCrashLoopBackOff:
		return processRules(f, "The Pod is in CrashLoopBackOff: the container kept exiting after each start, "+
			"and the kubelet restarted it with increasing back-off")
	case autopsy.CauseError:
		return processRules(f, "The container terminated with an error")
	case autopsy.CauseImagePullBackOff:
		return imageRules(f)
	case autopsy.CauseProbeFailure:
		return probeRules(f)
	default:
		return unknownRules(f)
	}
}

func oomRules(f facts) sections {
	limit := "the memory limit"
	if f.memLimit != "" {
		limit += " of " + f.memLimit
	}
	return sections{
		summary: sentence("The container was terminated by the kernel out-of-memory killer (reason OOMKilled)"+
			f.detail(), withFallback(f, f.status)...),
		rootCause: sentence("The memory use of the container exceeded "+limit+", so the kernel ended the process",
			withFallback(f, f.limitSpec, f.status)...),
		remediation: strings.Join([]string{
			bullet("Consider raising "+limit+" above the observed peak usage", f.limitSpec),
			bullet("Check what keeps memory growing, such as caches, unbounded queues or leaks"),
			bullet("Confirm that no new OOMKilled exit appears in the container events after the change"),
		}, "\n"),
		actionItems: strings.Join([]string{
			action("Raise "+limit+" above the observed peak usage", f.limitSpec),
			action("Profile the workload under load to find the allocation that keeps growing"),
			action("Add an alert on container memory usage near the limit"),
		}, "\n"),
	}
}

// processRules covers CrashLoopBackOff and Error. Both are failures of the
// process itself, so they share the startup and log advice.
func processRules(f facts, opening string) sections {
	return sections{
		summary: sentence(opening+f.detail(), withFallback(f, f.status)...),
		rootCause: sentence("The process exits with an error, and the first error-looking log line records the failure",
			withFallback(f, f.errLog)...),
		remediation: strings.Join([]string{
			bullet("Read the previous container log (kubectl logs --previous) to find the first error line", f.errLog),
			bullet("Check the startup command, the entrypoint arguments and the config file path"),
			bullet("Confirm that the image starts and stays up when run with the same command outside the cluster"),
		}, "\n"),
		actionItems: strings.Join([]string{
			action("Correct the startup command or config path named in the error log", f.errLog),
			action("Confirm that the config files and secrets the process needs are mounted at the expected paths"),
			action("Check the previous container log after each change, before the next restart"),
		}, "\n"),
	}
}

func imageRules(f facts) sections {
	image := ""
	if f.image != "" {
		image = " (image " + f.image + ")"
	}
	return sections{
		summary: sentence("The container image could not be pulled, so the container never started"+image,
			withFallback(f, f.status)...),
		rootCause: sentence("The image reference does not resolve to a pullable image, so the image name, "+
			"the tag or the registry access rights are probably wrong", withFallback(f, f.imageSpec, f.warning)...),
		remediation: strings.Join([]string{
			bullet("Confirm that the image name and tag exist in the registry", f.imageSpec),
			bullet("Check the registry address and access rights, and set imagePullSecrets for a private registry", f.warning),
			bullet("Consider pulling the image by hand on a node to confirm that the reference resolves"),
		}, "\n"),
		actionItems: strings.Join([]string{
			action("Correct the image name or tag in the workload spec", f.imageSpec),
			action("Confirm the credentials referenced by the imagePullSecrets of the Pod", f.warning),
			action("Pin the image to a tag that exists in the registry, not a moving or misspelled tag"),
		}, "\n"),
	}
}

func probeRules(f facts) sections {
	return sections{
		summary: sentence("The kubelet restarted the container after failed probes"+f.detail(),
			withFallback(f, f.warning, f.status)...),
		rootCause: sentence("The probe did not get a timely success response, because the probe path, port or timing "+
			"is wrong, or the application starts more slowly than the probe allows", withFallback(f, f.warning, f.status)...),
		remediation: strings.Join([]string{
			bullet("Check that the probe path and port respond from inside the container", f.warning),
			bullet("Consider a larger initialDelaySeconds or timeoutSeconds, based on the measured start-up time"),
			bullet("Confirm that the probe is still needed, because a failing probe restarts a container that may be healthy"),
		}, "\n"),
		actionItems: strings.Join([]string{
			action("Verify that the probe path and port return a success code", f.warning),
			action("Tune initialDelaySeconds and timeoutSeconds from the measured start-up time"),
			action("Review the failureThreshold of the probe"),
		}, "\n"),
	}
}

func unknownRules(f facts) sections {
	return sections{
		summary: sentence("The recorded evidence does not establish a single cause of failure",
			withFallback(f, f.status)...),
		rootCause: sentence("The evidence is incomplete, so the root cause cannot be confirmed from the recorded data",
			withFallback(f, f.errLog, f.warning, f.status)...),
		remediation: strings.Join([]string{
			bullet("Consider running kubectl describe pod and kubectl logs --previous to collect more information"),
			bullet("Compare the events and container states with the time of death"),
		}, "\n"),
		actionItems: strings.Join([]string{
			action("Collect kubectl describe pod output and previous container logs for this Pod"),
			action("Confirm the root cause once the missing data is collected"),
			action("Keep event history long enough to review failures like this one"),
		}, "\n"),
	}
}
