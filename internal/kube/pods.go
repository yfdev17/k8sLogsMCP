package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrNotFound is returned when no Service or workload has the requested name.
var ErrNotFound = errors.New("no Service, Deployment, StatefulSet or DaemonSet with this name")

// Selection is the set of pods that belong to a service.
type Selection struct {
	// Kind is the object the selector came from: Service, Deployment,
	// StatefulSet, DaemonSet, or LabelSelector when given explicitly.
	Kind      string
	Name      string
	Namespace string
	Selector  string
	Pods      []corev1.Pod
}

// AppLabels are the pod labels tried, in order, when name matches no object.
var AppLabels = []string{"app", "app.kubernetes.io/name"}

// SelectPods finds the pods of a service. With a non-empty labelSelector it is
// used as is. Otherwise name is looked up as a Service, then as a Deployment,
// StatefulSet and DaemonSet, and that object's pod selector is used. Kinds the
// caller may not read are skipped. If no object matches, pods labelled
// app=<name> (or app.kubernetes.io/name=<name>) are used, which groups
// blue/green deployments of one app.
func (c *Cluster) SelectPods(ctx context.Context, namespace, name, labelSelector string) (*Selection, error) {
	sel := &Selection{Kind: "LabelSelector", Name: name, Namespace: namespace, Selector: labelSelector}
	if labelSelector == "" {
		kind, ls, err := c.findSelector(ctx, namespace, name)
		if errors.Is(err, ErrNotFound) {
			return c.selectByAppLabel(ctx, sel, err)
		}
		if err != nil {
			return nil, err
		}
		s, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return nil, fmt.Errorf("%s %s/%s: invalid selector: %w", kind, namespace, name, err)
		}
		if s.Empty() {
			return nil, fmt.Errorf("%s %s/%s has no pod selector; pass label_selector explicitly", kind, namespace, name)
		}
		sel.Kind, sel.Selector = kind, s.String()
	}

	list, err := c.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.Selector})
	if err != nil {
		return nil, fmt.Errorf("list pods in %q with selector %q: %w", namespace, sel.Selector, err)
	}
	sel.Pods = list.Items
	sort.Slice(sel.Pods, func(i, j int) bool { return sel.Pods[i].Name < sel.Pods[j].Name })
	return sel, nil
}

func (c *Cluster) findSelector(ctx context.Context, ns, name string) (string, *metav1.LabelSelector, error) {
	apps := c.Client.AppsV1()
	lookups := []struct {
		kind string
		get  func() (*metav1.LabelSelector, error)
	}{
		{"Service", func() (*metav1.LabelSelector, error) {
			o, err := c.Client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return &metav1.LabelSelector{MatchLabels: o.Spec.Selector}, nil
		}},
		{"Deployment", func() (*metav1.LabelSelector, error) {
			o, err := apps.Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return o.Spec.Selector, nil
		}},
		{"StatefulSet", func() (*metav1.LabelSelector, error) {
			o, err := apps.StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return o.Spec.Selector, nil
		}},
		{"DaemonSet", func() (*metav1.LabelSelector, error) {
			o, err := apps.DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}
			return o.Spec.Selector, nil
		}},
	}

	var forbidden []string
	for _, l := range lookups {
		ls, err := l.get()
		switch {
		case apierrors.IsNotFound(err):
			continue
		case apierrors.IsForbidden(err):
			forbidden = append(forbidden, l.kind)
			continue
		case err != nil:
			return "", nil, fmt.Errorf("get %s %s/%s: %w", l.kind, ns, name, err)
		}
		return l.kind, ls, nil
	}
	if len(forbidden) > 0 {
		return "", nil, fmt.Errorf("%s/%s: %w (no permission to read %s)", ns, name, ErrNotFound, strings.Join(forbidden, ", "))
	}
	return "", nil, fmt.Errorf("%s/%s: %w", ns, name, ErrNotFound)
}

func (c *Cluster) selectByAppLabel(ctx context.Context, sel *Selection, notFound error) (*Selection, error) {
	for _, key := range AppLabels {
		s := key + "=" + sel.Name
		list, err := c.Client.CoreV1().Pods(sel.Namespace).List(ctx, metav1.ListOptions{LabelSelector: s})
		if err != nil {
			return nil, fmt.Errorf("list pods in %q with selector %q: %w", sel.Namespace, s, err)
		}
		if len(list.Items) > 0 {
			sel.Kind, sel.Selector, sel.Pods = "AppLabel", s, list.Items
			sort.Slice(sel.Pods, func(i, j int) bool { return sel.Pods[i].Name < sel.Pods[j].Name })
			return sel, nil
		}
	}
	return nil, fmt.Errorf("%w; no pods labelled %s=%s either", notFound, strings.Join(AppLabels, " or "), sel.Name)
}
