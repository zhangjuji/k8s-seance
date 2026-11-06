package k8s

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestEventsFor(t *testing.T) {
	ev := func(name, involved, uid string) corev1.Event {
		return corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: involved, UID: types.UID(uid)},
		}
	}
	items := []corev1.Event{
		ev("same-uid", "web", "uid-new"),
		ev("old-uid", "web", "uid-old"),
		ev("no-uid", "web", ""),
		ev("other-pod", "api", "uid-api"),
	}

	tests := []struct {
		name    string
		podName string
		podUID  string
		want    []string
	}{
		{name: "name and uid match, uid-less event kept", podName: "web", podUID: "uid-new", want: []string{"same-uid", "no-uid"}},
		{name: "pod without uid matches by name", podName: "web", podUID: "", want: []string{"same-uid", "old-uid", "no-uid"}},
		{name: "other name excluded", podName: "api", podUID: "uid-api", want: []string{"other-pod"}},
		{name: "no event for name", podName: "missing", podUID: "uid-x", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: tt.podName, UID: types.UID(tt.podUID)}}
			var got []string
			for _, e := range eventsFor(pod, items) {
				got = append(got, e.Name)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("eventsFor() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSummaryFamily: the list row carries the family from the revision labels
// (Requirement 18.2), so a return can be matched to a tombstone of its family.
func TestSummaryFamily(t *testing.T) {
	yes := true
	pod := func(kind, owner string, labels map[string]string) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", Labels: labels}}
		if kind != "" {
			p.OwnerReferences = []metav1.OwnerReference{{Kind: kind, Name: owner, Controller: &yes}}
		}
		return p
	}
	tests := []struct {
		pod  *corev1.Pod
		want string
	}{
		{pod("ReplicaSet", "web-5c6d7f8b9", map[string]string{labelPodTemplateHash: "5c6d7f8b9"}), "Deployment/web"},
		{pod("StatefulSet", "db", map[string]string{labelControllerRevision: "db-6f9c"}), "StatefulSet/db"},
		{pod("ReplicaSet", "lonely", nil), ""},
		{pod("", "", nil), ""},
	}
	for _, tt := range tests {
		if got := summarize(tt.pod).Family; got != tt.want {
			t.Errorf("Family = %q, want %q", got, tt.want)
		}
	}
}
