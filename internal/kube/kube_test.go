package kube

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A role that may read pods and deployments but not services, statefulsets or
// daemonsets, with blue/green deployments sharing app=billing.
func TestSelectPodsRestrictedRole(t *testing.T) {
	cs := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "billing-green", Namespace: "ns"},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "billing", "version": "green"}}},
		},
		pod("billing-blue-1", map[string]string{"app": "billing", "version": "blue"}, "billing"),
		pod("billing-green-1", map[string]string{"app": "billing", "version": "green"}, "billing"),
		pod("billing-worker-1", map[string]string{"app": "billing-worker"}, "worker"),
	)
	for _, res := range []string{"services", "statefulsets", "daemonsets"} {
		cs.PrependReactor("get", res, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: res}, "", errors.New("rbac"))
		})
	}
	c := &Cluster{Client: cs, Namespace: "ns"}

	tests := []struct {
		svc, wantKind string
		wantPods      []string
	}{
		{"billing-green", "Deployment", []string{"billing-green-1"}},
		{"billing", "AppLabel", []string{"billing-blue-1", "billing-green-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.svc, func(t *testing.T) {
			sel, err := c.SelectPods(t.Context(), "ns", tt.svc, "")
			if err != nil {
				t.Fatalf("SelectPods(%q): %v", tt.svc, err)
			}
			var got []string
			for _, p := range sel.Pods {
				got = append(got, p.Name)
			}
			if sel.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", sel.Kind, tt.wantKind)
			}
			if diff := cmp.Diff(tt.wantPods, got); diff != "" {
				t.Errorf("pods mismatch (-want +got):\n%s", diff)
			}
		})
	}

	if _, err := c.SelectPods(t.Context(), "ns", "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectPods(missing) error = %v, want ErrNotFound", err)
	}
}

func pod(name string, lbl map[string]string, containers ...string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: lbl}}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

func testCluster(objs ...runtime.Object) *Cluster {
	return &Cluster{Client: fake.NewClientset(objs...), Context: "test", Namespace: "ns"}
}

func TestSelectPods(t *testing.T) {
	api := map[string]string{"app": "api"}
	worker := map[string]string{"app": "worker"}
	c := testCluster(
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
			Spec:       corev1.ServiceSpec{Selector: api},
		},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "ns"}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "ns"},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: worker}},
		},
		pod("api-2", api, "app"),
		pod("api-1", api, "app", "sidecar"),
		pod("worker-1", worker, "app"),
	)

	tests := []struct {
		name, svc, selector string
		wantKind            string
		wantPods            []string
		wantErr             bool
	}{
		{name: "service", svc: "api", wantKind: "Service", wantPods: []string{"api-1", "api-2"}},
		{name: "deployment fallback", svc: "worker", wantKind: "Deployment", wantPods: []string{"worker-1"}},
		{name: "explicit selector", svc: "x", selector: "app=worker", wantKind: "LabelSelector", wantPods: []string{"worker-1"}},
		{name: "service without selector", svc: "external", wantErr: true},
		{name: "not found", svc: "missing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := c.SelectPods(t.Context(), "ns", tt.svc, tt.selector)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SelectPods(%q) = nil error, want error", tt.svc)
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectPods(%q): %v", tt.svc, err)
			}
			var got []string
			for _, p := range sel.Pods {
				got = append(got, p.Name)
			}
			if sel.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", sel.Kind, tt.wantKind)
			}
			if diff := cmp.Diff(tt.wantPods, got); diff != "" {
				t.Errorf("pods mismatch (-want +got):\n%s", diff)
			}
		})
	}

	if _, err := c.SelectPods(t.Context(), "ns", "missing", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectPods(missing) error = %v, want ErrNotFound", err)
	}
}

func TestStreams(t *testing.T) {
	p := pod("p", nil, "app", "sidecar")
	p.Spec.InitContainers = []corev1.Container{{Name: "migrate"}}
	pods := []corev1.Pod{*p}

	tests := []struct {
		name string
		opts LogOptions
		want []Stream
	}{
		{"all", LogOptions{}, []Stream{{"p", "app"}, {"p", "sidecar"}}},
		{"with init", LogOptions{IncludeInit: true}, []Stream{{"p", "migrate"}, {"p", "app"}, {"p", "sidecar"}}},
		{"one container", LogOptions{Container: "sidecar"}, []Stream{{"p", "sidecar"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, Streams(pods, tt.opts)); diff != "" {
				t.Errorf("Streams mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The fake clientset answers every log request with "fake logs".
func TestFetchLogs(t *testing.T) {
	c := testCluster(pod("p", nil, "app"))
	streams := []Stream{{"p", "app"}}

	got := c.FetchLogs(t.Context(), "ns", streams, LogOptions{}, nil, 0)
	if got[0].Err != nil || got[0].Text != "fake logs" {
		t.Errorf("FetchLogs = %+v, want text %q", got[0], "fake logs")
	}

	got = c.FetchLogs(t.Context(), "ns", streams, LogOptions{}, regexp.MustCompile("error"), 0)
	if got[0].Lines != 0 {
		t.Errorf("FetchLogs with non-matching filter kept %d lines, want 0", got[0].Lines)
	}
}

func TestDownloadLogs(t *testing.T) {
	c := testCluster(pod("p", nil, "app"))
	dir := filepath.Join(t.TempDir(), "svc", "run")

	files, err := c.DownloadLogs(t.Context(), "ns", []Stream{{"p", "app"}}, LogOptions{Previous: true}, dir)
	if err != nil {
		t.Fatalf("DownloadLogs: %v", err)
	}
	want := filepath.Join(dir, "p_app.previous.log")
	if files[0].Path != want || files[0].Error != "" {
		t.Fatalf("DownloadLogs = %+v, want path %q", files[0], want)
	}
	data, err := os.ReadFile(want)
	if err != nil || string(data) != "fake logs" {
		t.Errorf("file content = %q, %v; want %q", data, err, "fake logs")
	}
}

func TestManagerContexts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	const cfg = `apiVersion: v1
kind: Config
current-context: prod
clusters:
- name: c1
  cluster: {server: https://prod.example:6443}
- name: c2
  cluster: {server: https://dev.example:6443}
users:
- name: u
  user: {token: t}
contexts:
- name: prod
  context: {cluster: c1, user: u, namespace: shop}
- name: dev
  context: {cluster: c2, user: u}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, "")

	got, err := m.Contexts("")
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	want := []ContextInfo{
		{Name: "dev", Cluster: "c2", Server: "https://dev.example:6443", User: "u"},
		{Name: "prod", Cluster: "c1", Server: "https://prod.example:6443", User: "u", Namespace: "shop", Current: true},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Contexts mismatch (-want +got):\n%s", diff)
	}

	c, err := m.Cluster("", "")
	if err != nil {
		t.Fatalf("Cluster: %v", err)
	}
	if c.Context != "prod" || c.Namespace != "shop" {
		t.Errorf("Cluster = {%q, %q}, want {prod, shop}", c.Context, c.Namespace)
	}
	if c2, _ := m.Cluster("", ""); c2 != c {
		t.Error("Cluster is not cached")
	}
}
