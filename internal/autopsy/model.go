// Package autopsy defines the data contract between the read-only collector
// and the rest of the application. It must not import client-go.
package autopsy

import "time"

// CauseOfDeath is the classifier output for a dead or crash-looping Pod.
type CauseOfDeath string

const (
	CauseOOMKilled        CauseOfDeath = "OOMKilled"
	CauseCrashLoopBackOff CauseOfDeath = "CrashLoopBackOff"
	CauseImagePullBackOff CauseOfDeath = "ImagePullBackOff"
	CauseError            CauseOfDeath = "Error"
	CauseProbeFailure     CauseOfDeath = "ProbeFailure"
	CauseUnknown          CauseOfDeath = "Unknown"
)

// ContainerState is a simplified view of a container state (running, waiting or terminated).
type ContainerState struct {
	Kind       string     `json:"kind"` // "running" | "waiting" | "terminated"
	Reason     string     `json:"reason,omitempty"`
	Message    string     `json:"message,omitempty"`
	ExitCode   *int32     `json:"exitCode,omitempty"` // nil means no exit code
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// ContainerAutopsy holds the collected facts about one container.
type ContainerAutopsy struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	RestartCount    int32             `json:"restartCount"`
	State           ContainerState    `json:"state"`
	LastState       *ContainerState   `json:"lastState,omitempty"`
	Limits          map[string]string `json:"limits,omitempty"`
	Requests        map[string]string `json:"requests,omitempty"`
	LogTail         []string          `json:"logTail,omitempty"`
	PreviousLogTail []string          `json:"previousLogTail,omitempty"` // nil means "no record" (Req 2.5)
}

// EventRecord is a Kubernetes Event that involves the Pod.
type EventRecord struct {
	Type           string     `json:"type"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Count          int32      `json:"count"`
	FirstTimestamp *time.Time `json:"firstTimestamp,omitempty"`
	LastTimestamp  *time.Time `json:"lastTimestamp,omitempty"`
}

// PodAutopsy is the unified collected snapshot of one Pod.
type PodAutopsy struct {
	Namespace   string             `json:"namespace"`
	Name        string             `json:"name"`
	UID         string             `json:"uid"`
	CreatedAt   time.Time          `json:"createdAt"`
	Phase       string             `json:"phase"`
	NodeName    string             `json:"nodeName,omitempty"`
	OwnerKind   string             `json:"ownerKind,omitempty"`
	OwnerName   string             `json:"ownerName,omitempty"`
	Containers  []ContainerAutopsy `json:"containers"`
	Events      []EventRecord      `json:"events,omitempty"`
	CollectedAt time.Time          `json:"collectedAt"`
	Source      string             `json:"source"` // "cluster" | "fixture"
	// Lineage is the family of the Pod (Requirement 16): siblings of the same
	// revision and Pods of other revisions. Nil when the Pod has no controller
	// family or the Pod list could not be read.
	Lineage *Lineage `json:"lineage,omitempty"`
}

// Classification is the deterministic death-cause result.
type Classification struct {
	Cause        CauseOfDeath   `json:"cause"`
	Secondary    []CauseOfDeath `json:"secondary,omitempty"`
	LastExitCode *int32         `json:"lastExitCode,omitempty"`
}

// Evidence is a numbered fact that the spirit must cite, for example "E3".
type Evidence struct {
	ID        string     `json:"id"`   // "E1"
	Kind      string     `json:"kind"` // "status" | "event" | "lineage" | "log" | "spec"
	Text      string     `json:"text"`
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

// SpiritSummary is one row in the spirit list (Req 1.2).
// Cause is empty when produced by the k8s list call. Classify a PodAutopsy to get it.
type SpiritSummary struct {
	Namespace    string       `json:"namespace"`
	Name         string       `json:"name"`
	Cause        CauseOfDeath `json:"cause,omitempty"`
	RestartCount int32        `json:"restartCount"`
	LastDeath    *time.Time   `json:"lastDeath,omitempty"`
	// Memory is how long the cluster still keeps the spirit's events
	// (Requirement 14). The k8s list leaves it nil. The web sources fill it.
	Memory *Memory `json:"memory,omitempty"`
	// Family is the controller family as "Kind/name", for example
	// "Deployment/web" (Requirement 18.2). Empty when the Pod has none. The
	// cluster list reads the revision labels; the fixtures use FamilyOf.
	Family string `json:"family,omitempty"`
}
