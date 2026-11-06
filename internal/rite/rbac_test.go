package rite

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// TestResurrectRBACIsNamespacedAndMinimal reads demo/rbac-resurrect.yaml and
// checks that it is a Role (not a ClusterRole) in seance-demo, that it grants
// no delete, no secrets and no Pod writes, and that it covers every write
// Caster makes.
func TestResurrectRBACIsNamespacedAndMinimal(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "demo", "rbac-resurrect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var role *rbacv1.Role
	var binding *rbacv1.RoleBinding
	for _, doc := range bytes.Split(raw, []byte("\n---\n")) {
		var meta struct{ Kind string }
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatal(err)
		}
		switch meta.Kind {
		case "Role":
			role = &rbacv1.Role{}
			if err := yaml.UnmarshalStrict(doc, role); err != nil {
				t.Fatal(err)
			}
		case "RoleBinding":
			binding = &rbacv1.RoleBinding{}
			if err := yaml.UnmarshalStrict(doc, binding); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected kind %q: only a Role and a RoleBinding are allowed", meta.Kind)
		}
	}
	if role == nil || binding == nil {
		t.Fatal("want one Role and one RoleBinding")
	}
	if role.Namespace != "seance-demo" || binding.Namespace != "seance-demo" {
		t.Errorf("namespaces = %q, %q, want seance-demo", role.Namespace, binding.Namespace)
	}
	if binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != role.Name {
		t.Errorf("roleRef = %+v", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "k8s-seance" || binding.Subjects[0].Namespace != "seance-demo" {
		t.Errorf("subjects = %+v", binding.Subjects)
	}

	granted := map[string]bool{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			for _, v := range r.Verbs {
				if v == "*" || res == "*" || slices.Contains([]string{"delete", "deletecollection", "escalate", "bind", "impersonate"}, v) {
					t.Errorf("rule grants %s on %s", v, res)
				}
				if res == "secrets" || res == "pods" || res == "pods/exec" || res == "pods/eviction" {
					t.Errorf("rule grants %s on %s", v, res)
				}
				granted[v+" "+res] = true
			}
		}
	}
	for _, need := range []string{
		"patch deployments", "patch statefulsets", "patch daemonsets", "get deployments",
		"list replicasets", "get configmaps", "create configmaps", "update configmaps",
	} {
		if !granted[need] {
			t.Errorf("the Role does not grant %q, which Caster needs", need)
		}
	}
}
