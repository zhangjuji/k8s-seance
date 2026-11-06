package resurrect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"sigs.k8s.io/yaml"
)

// ActionKind is what a castable spell does to the cluster (Requirement 19.2).
type ActionKind string

const (
	// ActionPatch applies a strategic merge patch to the target: a raised
	// memory limit, or probe timing given by the user.
	ActionPatch ActionKind = "patch"
	// ActionSetImage replaces the image tag of one container with a tag the user gives.
	ActionSetImage ActionKind = "set-image"
	// ActionRollback replaces the Deployment's Pod template with the template of
	// the ReplicaSet that is still alive, like kubectl rollout undo --to-revision.
	ActionRollback ActionKind = "rollback"
	// ActionConfigMapMount creates or updates a ConfigMap with content the user
	// pastes, then patches the target to mount it.
	ActionConfigMapMount ActionKind = "configmap-mount"
)

// Input names of an Action.
const (
	InputTag              = "tag"
	InputConfig           = "config"
	InputTimeoutSeconds   = "timeoutSeconds"
	InputFailureThreshold = "failureThreshold"
)

// Bounds on user input.
const (
	// MaxConfigBytes bounds the pasted configuration file. A ConfigMap can hold
	// 1 MiB; a demo config needs far less.
	MaxConfigBytes   = 64 << 10
	maxProbeTimeout  = 300
	maxProbeFailures = 100
)

// Input is one value the user must give before the spell can be cast, because
// the evidence cannot give it. Kind is "text", "textarea" or "number".
type Input struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Min  int    `json:"min,omitempty"`
	Max  int    `json:"max,omitempty"`
}

// Action is the machine-executable form of a spell. Only internal/rite runs it.
// Spells whose fix needs a secret, a debugging session or a change outside the
// Pod's controller have no Action and can only be read.
type Action struct {
	Kind ActionKind `json:"kind"`
	// TargetKind is Deployment, StatefulSet or DaemonSet.
	TargetKind string `json:"targetKind"`
	TargetName string `json:"targetName"`
	Namespace  string `json:"namespace"`
	Container  string `json:"container,omitempty"`
	// Patch is the strategic merge patch in YAML, for ActionPatch without
	// inputs and for ActionConfigMapMount.
	Patch string `json:"patch,omitempty"`
	// Probe is livenessProbe, readinessProbe or startupProbe, for a probe patch.
	Probe string `json:"probe,omitempty"`
	// Repo and CurrentTag are the image of Container, for ActionSetImage.
	Repo       string `json:"repo,omitempty"`
	CurrentTag string `json:"currentTag,omitempty"`
	// ConfigMap, ConfigKey and MountPath are for ActionConfigMapMount.
	ConfigMap string `json:"configMap,omitempty"`
	ConfigKey string `json:"configKey,omitempty"`
	MountPath string `json:"mountPath,omitempty"`
	// HashLabel and Revision name the living ReplicaSet, for ActionRollback.
	HashLabel string `json:"hashLabel,omitempty"`
	Revision  string `json:"revision,omitempty"`
	// Inputs are the values the user gives, in form order.
	Inputs []Input `json:"inputs,omitempty"`
}

// Bound is an Action with the user's inputs checked and filled in. It is what
// internal/rite executes. Every field is ready to send to the API server.
type Bound struct {
	Kind       ActionKind `json:"kind"`
	Namespace  string     `json:"namespace"`
	TargetKind string     `json:"targetKind"`
	TargetName string     `json:"targetName"`
	// Patch is a strategic merge patch in JSON for the target, empty for a rollback.
	Patch []byte `json:"patch,omitempty"`
	// ConfigMap is written before Patch, for ActionConfigMapMount.
	ConfigMap *ConfigMapData `json:"configMap,omitempty"`
	// Rollback names the living ReplicaSet, for ActionRollback.
	Rollback *RollbackRef `json:"rollback,omitempty"`
}

// ConfigMapData is one key of a ConfigMap.
type ConfigMapData struct {
	Name string `json:"name"`
	Key  string `json:"key"`
	Data string `json:"data"`
}

// RollbackRef is the ReplicaSet revision a rollback returns to.
type RollbackRef struct {
	HashLabel string `json:"hashLabel"`
	Revision  string `json:"revision"`
}

// InputError says which input was rejected and why. Code is one of "missing",
// "invalid", "same", "too_large" or "range".
type InputError struct {
	Input string
	Code  string
}

func (e *InputError) Error() string { return fmt.Sprintf("input %s: %s", e.Input, e.Code) }

// castableKinds are the controllers a spell may be cast on.
var castableKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

// imageTag is the grammar of an image tag.
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// objectName is a DNS-1123 subdomain, the rule for Deployment and ConfigMap names.
var objectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Bind checks the user's inputs and returns the action ready to cast. It
// re-checks the target too, so a hand-made Action cannot reach other objects.
func (a *Action) Bind(in map[string]string) (Bound, error) {
	if a == nil {
		return Bound{}, fmt.Errorf("resurrect: this spell cannot be cast")
	}
	if !castableKinds[a.TargetKind] {
		return Bound{}, fmt.Errorf("resurrect: target kind %q cannot be cast on", a.TargetKind)
	}
	if !dnsLabel.MatchString(a.Namespace) || len(a.Namespace) > 63 {
		return Bound{}, fmt.Errorf("resurrect: invalid namespace %q", a.Namespace)
	}
	if !objectName.MatchString(a.TargetName) || len(a.TargetName) > 253 {
		return Bound{}, fmt.Errorf("resurrect: invalid target name %q", a.TargetName)
	}
	b := Bound{Kind: a.Kind, Namespace: a.Namespace, TargetKind: a.TargetKind, TargetName: a.TargetName}
	var err error
	switch a.Kind {
	case ActionPatch:
		if a.Probe != "" {
			b.Patch, err = a.probePatch(in)
		} else {
			b.Patch, err = patchJSON(a.Patch)
		}
	case ActionSetImage:
		b.Patch, err = a.imagePatch(in)
	case ActionConfigMapMount:
		b.ConfigMap, err = a.configData(in)
		if err == nil {
			b.Patch, err = patchJSON(a.Patch)
		}
	case ActionRollback:
		if a.TargetKind != "Deployment" || a.HashLabel == "" || a.Revision == "" {
			return Bound{}, fmt.Errorf("resurrect: rollback needs a Deployment and a living revision")
		}
		b.Rollback = &RollbackRef{HashLabel: a.HashLabel, Revision: a.Revision}
	default:
		return Bound{}, fmt.Errorf("resurrect: unknown action %q", a.Kind)
	}
	if err != nil {
		return Bound{}, err
	}
	return b, nil
}

// Fingerprint identifies a bound action, inputs included. A real cast must
// follow a successful rehearsal with the same fingerprint.
func (b Bound) Fingerprint() string {
	data, _ := json.Marshal(b)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// patchJSON converts a YAML strategic merge patch to JSON.
func patchJSON(y string) ([]byte, error) {
	if strings.TrimSpace(y) == "" {
		return nil, fmt.Errorf("resurrect: the spell has no patch")
	}
	j, err := yaml.YAMLToJSON([]byte(y))
	if err != nil {
		return nil, fmt.Errorf("resurrect: patch: %w", err)
	}
	return j, nil
}

// containerPatch is the strategic merge patch of one container in a Pod template.
func containerPatch(container map[string]any) ([]byte, error) {
	return json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
		"containers": []any{container},
	}}}})
}

func (a *Action) imagePatch(in map[string]string) ([]byte, error) {
	tag := strings.TrimSpace(in[InputTag])
	switch {
	case tag == "":
		return nil, &InputError{Input: InputTag, Code: "missing"}
	case !imageTag.MatchString(tag):
		return nil, &InputError{Input: InputTag, Code: "invalid"}
	case tag == a.CurrentTag:
		return nil, &InputError{Input: InputTag, Code: "same"}
	}
	if a.Repo == "" || !dnsLabel.MatchString(a.Container) {
		return nil, fmt.Errorf("resurrect: the spell names no image")
	}
	return containerPatch(map[string]any{"name": a.Container, "image": a.Repo + ":" + tag})
}

func (a *Action) probePatch(in map[string]string) ([]byte, error) {
	switch a.Probe {
	case "livenessProbe", "readinessProbe", "startupProbe":
	default:
		return nil, fmt.Errorf("resurrect: unknown probe %q", a.Probe)
	}
	if !dnsLabel.MatchString(a.Container) {
		return nil, fmt.Errorf("resurrect: invalid container %q", a.Container)
	}
	timeout, err := intInput(in, InputTimeoutSeconds, maxProbeTimeout)
	if err != nil {
		return nil, err
	}
	failures, err := intInput(in, InputFailureThreshold, maxProbeFailures)
	if err != nil {
		return nil, err
	}
	return containerPatch(map[string]any{"name": a.Container, a.Probe: map[string]any{
		"timeoutSeconds": timeout, "failureThreshold": failures,
	}})
}

// intInput reads an integer from 1 to hi.
func intInput(in map[string]string, name string, hi int) (int, error) {
	s := strings.TrimSpace(in[name])
	if s == "" {
		return 0, &InputError{Input: name, Code: "missing"}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, &InputError{Input: name, Code: "invalid"}
	}
	if n < 1 || n > hi {
		return 0, &InputError{Input: name, Code: "range"}
	}
	return n, nil
}

func (a *Action) configData(in map[string]string) (*ConfigMapData, error) {
	data := in[InputConfig]
	switch {
	case strings.TrimSpace(data) == "":
		return nil, &InputError{Input: InputConfig, Code: "missing"}
	case len(data) > MaxConfigBytes:
		return nil, &InputError{Input: InputConfig, Code: "too_large"}
	case !utf8.ValidString(data) || strings.ContainsRune(data, 0):
		return nil, &InputError{Input: InputConfig, Code: "invalid"}
	}
	if !dnsLabel.MatchString(a.ConfigMap) || !configKey.MatchString(a.ConfigKey) {
		return nil, fmt.Errorf("resurrect: invalid ConfigMap %q key %q", a.ConfigMap, a.ConfigKey)
	}
	// Browsers send CRLF line ends from a textarea; the file should have LF.
	data = strings.ReplaceAll(data, "\r\n", "\n")
	return &ConfigMapData{Name: a.ConfigMap, Key: a.ConfigKey, Data: data}, nil
}

// action starts the Action of the spell for kind, or returns nil when the
// target cannot be cast on in place.
func (c *caster) action(kind ActionKind) *Action {
	t := c.sp.Target
	if !t.InPlace || !castableKinds[t.Kind] {
		return nil
	}
	return &Action{Kind: kind, TargetKind: t.Kind, TargetName: t.Name, Namespace: c.a.Namespace, Container: t.Container}
}
