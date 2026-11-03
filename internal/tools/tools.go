// Package tools registers the MCP tools of the k8s-logs server.
package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s-logs-mcp/internal/kube"
)

const (
	defaultTailLines = 200
	defaultMaxBytes  = 64 * 1024
)

// Config is what the tools need from the server.
type Config struct {
	Kube *kube.Manager
	// LogsDir is the root for downloaded logs when a call names no project;
	// files go to <LogsDir>/<service>/<timestamp>/.
	LogsDir string
	// Env names the environment (dev, test, stage) in project log paths:
	// <project_dir>/logs/<Env>/<timestamp>/. Empty means the context name.
	Env string
	// Namespace overrides the kubeconfig context namespace when a tool call
	// does not name one.
	Namespace string
}

// Register adds every tool to s.
func Register(s *mcp.Server, cfg Config) {
	h := &handlers{cfg: cfg}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_contexts",
		Description: "List contexts (clusters) in the kubeconfig file, the same file used by kubectl or OpenLens.",
		Annotations: readOnly,
	}, h.listContexts)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List namespaces of a cluster context.",
		Annotations: readOnly,
	}, h.listNamespaces)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_services",
		Description: "List what can be passed as `service` to the log tools in a namespace: apps (pods grouped by their app / " +
			"app.kubernetes.io/name label, e.g. one app spanning blue and green deployments), Services with their pod selectors, " +
			"and workloads (Deployments, StatefulSets, DaemonSets). Kinds the credentials may not read are listed in warnings.",
		Annotations: readOnly,
	}, h.listServices)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_service_pods",
		Description: "List all pods of a service with phase, readiness, restarts and containers.",
		Annotations: readOnly,
	}, h.listServicePods)

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_service_logs",
		Description: "Get logs from all pods (and all containers) of a service in one call. " +
			"`service` is resolved as a Service selector, else a Deployment/StatefulSet/DaemonSet name, " +
			"else the pod label app=<service> (covers all blue/green versions of an app). " +
			"By default returns the last 200 lines per container; use `since`, `filter` (regex) and `previous` to narrow down. " +
			"For complete logs use download_service_logs.",
		Annotations: readOnly,
	}, h.getServiceLogs)

	mcp.AddTool(s, &mcp.Tool{
		Name: "download_service_logs",
		Description: "Download complete logs of all pods of a service to local files and return the file paths. " +
			"When the user works in a project for this service (e.g. a folder named like the service inside the open " +
			"workspace), pass its absolute path as project_dir: files go to " +
			"<project_dir>/logs/<env>/<timestamp>/<pod>_<container>.log. Without project_dir they go to " +
			"<logs-dir>/<service>/<timestamp>/.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: false, DestructiveHint: new(false)},
	}, h.downloadServiceLogs)
}

type handlers struct{ cfg Config }

// Inputs. Embedded structs are flattened into one JSON object.

type target struct {
	Kubeconfig string `json:"kubeconfig,omitempty" jsonschema:"path to a kubeconfig file (as used by kubectl/OpenLens); defaults to the server --kubeconfig, $KUBECONFIG or ~/.kube/config"`
	Context    string `json:"context,omitempty" jsonschema:"kubeconfig context name; defaults to the current context"`
}

type nsTarget struct {
	target
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace; defaults to the server --namespace or the context namespace"`
}

type serviceTarget struct {
	nsTarget
	Service       string `json:"service" jsonschema:"name of the Service, Deployment, StatefulSet, DaemonSet or app label value"`
	LabelSelector string `json:"label_selector,omitempty" jsonschema:"explicit pod label selector, e.g. app=api,tier=web; skips lookup by name"`
}

type logParams struct {
	Container   string `json:"container,omitempty" jsonschema:"only this container; default is all containers of each pod"`
	IncludeInit bool   `json:"include_init,omitempty" jsonschema:"also read init containers"`
	Since       string `json:"since,omitempty" jsonschema:"only logs newer than this Go duration, e.g. 15m, 2h, 72h"`
	Previous    bool   `json:"previous,omitempty" jsonschema:"logs of the previous (crashed/restarted) container instance"`
	Timestamps  bool   `json:"timestamps,omitempty" jsonschema:"prefix each line with its RFC3339 timestamp"`
}

type getLogsInput struct {
	serviceTarget
	logParams
	TailLines *int64 `json:"tail_lines,omitempty" jsonschema:"last N lines per container (default 200; 0 = all, applied before filter)"`
	Filter    string `json:"filter,omitempty" jsonschema:"Go regexp; keep only matching lines, e.g. (?i)error|panic"`
	MaxBytes  int    `json:"max_bytes_per_container,omitempty" jsonschema:"keep at most this many bytes of the newest lines per container (default 65536)"`
}

type downloadInput struct {
	serviceTarget
	logParams
	TailLines  int64  `json:"tail_lines,omitempty" jsonschema:"last N lines per container; default 0 = complete logs"`
	ProjectDir string `json:"project_dir,omitempty" jsonschema:"absolute path of the project directory of this service; logs go to <project_dir>/logs/<env>/<timestamp>/"`
}

// Outputs.

type contextsOutput struct {
	Contexts []kube.ContextInfo `json:"contexts"`
}

type namespacesOutput struct {
	Context    string   `json:"context"`
	Namespaces []string `json:"namespaces"`
	Warning    string   `json:"warning,omitempty"`
}

type serviceInfo struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Selector map[string]string `json:"selector,omitempty"`
	Ports    []string          `json:"ports,omitempty"`
}

type workloadInfo struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Ready string `json:"ready"`
}

type appInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Pods    int    `json:"pods"`
	Running int    `json:"running"`
}

type servicesOutput struct {
	Context   string         `json:"context"`
	Namespace string         `json:"namespace"`
	Apps      []appInfo      `json:"apps"`
	Services  []serviceInfo  `json:"services"`
	Workloads []workloadInfo `json:"workloads"`
	Warnings  []string       `json:"warnings,omitempty"`
}

type podInfo struct {
	Name           string   `json:"name"`
	Phase          string   `json:"phase"`
	Ready          string   `json:"ready"`
	Restarts       int32    `json:"restarts"`
	Node           string   `json:"node,omitempty"`
	Age            string   `json:"age"`
	Containers     []string `json:"containers"`
	InitContainers []string `json:"init_containers,omitempty"`
}

type podsOutput struct {
	Context   string    `json:"context"`
	Namespace string    `json:"namespace"`
	Kind      string    `json:"resolved_kind"`
	Selector  string    `json:"selector"`
	Pods      []podInfo `json:"pods"`
}

type downloadOutput struct {
	Context   string            `json:"context"`
	Namespace string            `json:"namespace"`
	Kind      string            `json:"resolved_kind"`
	Selector  string            `json:"selector"`
	Dir       string            `json:"dir"`
	Files     []kube.FileResult `json:"files"`
	Failed    int               `json:"failed"`
}

func (h *handlers) listContexts(_ context.Context, _ *mcp.CallToolRequest, in target) (*mcp.CallToolResult, contextsOutput, error) {
	ctxs, err := h.cfg.Kube.Contexts(in.Kubeconfig)
	if err != nil {
		return nil, contextsOutput{}, err
	}
	return nil, contextsOutput{Contexts: ctxs}, nil
}

func (h *handlers) listNamespaces(ctx context.Context, _ *mcp.CallToolRequest, in target) (*mcp.CallToolResult, namespacesOutput, error) {
	c, err := h.cfg.Kube.Cluster(in.Kubeconfig, in.Context)
	if err != nil {
		return nil, namespacesOutput{}, err
	}
	list, err := c.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if apierrors.IsForbidden(err) {
		return nil, namespacesOutput{
			Context:    c.Context,
			Namespaces: []string{c.Namespace},
			Warning:    "no permission to list namespaces; returning the context namespace only",
		}, nil
	}
	if err != nil {
		return nil, namespacesOutput{}, fmt.Errorf("list namespaces: %w", err)
	}
	out := namespacesOutput{Context: c.Context, Namespaces: make([]string, 0, len(list.Items))}
	for _, ns := range list.Items {
		out.Namespaces = append(out.Namespaces, ns.Name)
	}
	sort.Strings(out.Namespaces)
	return nil, out, nil
}

func (h *handlers) listServices(ctx context.Context, _ *mcp.CallToolRequest, in nsTarget) (*mcp.CallToolResult, servicesOutput, error) {
	c, ns, err := h.cluster(in)
	if err != nil {
		return nil, servicesOutput{}, err
	}
	out := servicesOutput{Context: c.Context, Namespace: ns, Apps: []appInfo{}, Services: []serviceInfo{}, Workloads: []workloadInfo{}}

	// Each list is optional: a restricted role (e.g. pods + logs only) still
	// gets whatever it may read, with a warning for the rest.
	skip := func(what string, err error) bool {
		if apierrors.IsForbidden(err) {
			out.Warnings = append(out.Warnings, "no permission to list "+what)
			return true
		}
		return false
	}

	pods, err := c.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	switch {
	case skip("pods", err):
	case err != nil:
		return nil, out, fmt.Errorf("list pods: %w", err)
	default:
		out.Apps = groupByApp(pods.Items)
	}

	svcs, err := c.Client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	switch {
	case skip("services", err):
	case err != nil:
		return nil, out, fmt.Errorf("list services: %w", err)
	default:
		for _, s := range svcs.Items {
			si := serviceInfo{Name: s.Name, Type: string(s.Spec.Type), Selector: s.Spec.Selector}
			for _, p := range s.Spec.Ports {
				si.Ports = append(si.Ports, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
			}
			out.Services = append(out.Services, si)
		}
	}

	apps := c.Client.AppsV1()
	deps, err := apps.Deployments(ns).List(ctx, metav1.ListOptions{})
	switch {
	case skip("deployments", err):
	case err != nil:
		return nil, out, fmt.Errorf("list deployments: %w", err)
	default:
		for _, d := range deps.Items {
			out.Workloads = append(out.Workloads, workloadInfo{"Deployment", d.Name, fmt.Sprintf("%d/%d", d.Status.ReadyReplicas, d.Status.Replicas)})
		}
	}
	sts, err := apps.StatefulSets(ns).List(ctx, metav1.ListOptions{})
	switch {
	case skip("statefulsets", err):
	case err != nil:
		return nil, out, fmt.Errorf("list statefulsets: %w", err)
	default:
		for _, s := range sts.Items {
			out.Workloads = append(out.Workloads, workloadInfo{"StatefulSet", s.Name, fmt.Sprintf("%d/%d", s.Status.ReadyReplicas, s.Status.Replicas)})
		}
	}
	dss, err := apps.DaemonSets(ns).List(ctx, metav1.ListOptions{})
	switch {
	case skip("daemonsets", err):
	case err != nil:
		return nil, out, fmt.Errorf("list daemonsets: %w", err)
	default:
		for _, d := range dss.Items {
			out.Workloads = append(out.Workloads, workloadInfo{"DaemonSet", d.Name, fmt.Sprintf("%d/%d", d.Status.NumberReady, d.Status.DesiredNumberScheduled)})
		}
	}
	return nil, out, nil
}

// groupByApp counts pods per value of the first kube.AppLabels label they carry.
func groupByApp(pods []corev1.Pod) []appInfo {
	byName := map[string]*appInfo{}
	for _, p := range pods {
		for _, key := range kube.AppLabels {
			v, ok := p.Labels[key]
			if !ok {
				continue
			}
			a, ok := byName[v]
			if !ok {
				a = &appInfo{Name: v, Label: key}
				byName[v] = a
			}
			a.Pods++
			if p.Status.Phase == corev1.PodRunning {
				a.Running++
			}
			break
		}
	}
	out := make([]appInfo, 0, len(byName))
	for _, a := range byName {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (h *handlers) listServicePods(ctx context.Context, _ *mcp.CallToolRequest, in serviceTarget) (*mcp.CallToolResult, podsOutput, error) {
	c, sel, err := h.selectPods(ctx, in)
	if err != nil {
		return nil, podsOutput{}, err
	}
	out := podsOutput{Context: c.Context, Namespace: sel.Namespace, Kind: sel.Kind, Selector: sel.Selector, Pods: []podInfo{}}
	for _, p := range sel.Pods {
		out.Pods = append(out.Pods, describePod(p))
	}
	return nil, out, nil
}

func (h *handlers) getServiceLogs(ctx context.Context, _ *mcp.CallToolRequest, in getLogsInput) (*mcp.CallToolResult, any, error) {
	opts, err := in.options()
	if err != nil {
		return nil, nil, err
	}
	opts.TailLines = defaultTailLines
	if in.TailLines != nil {
		if *in.TailLines < 0 {
			return nil, nil, errors.New("tail_lines must be >= 0")
		}
		opts.TailLines = *in.TailLines
	}
	var filter *regexp.Regexp
	if in.Filter != "" {
		if filter, err = regexp.Compile(in.Filter); err != nil {
			return nil, nil, fmt.Errorf("invalid filter: %w", err)
		}
	}
	maxBytes := in.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	c, sel, err := h.selectPods(ctx, in.serviceTarget)
	if err != nil {
		return nil, nil, err
	}
	streams := kube.Streams(sel.Pods, opts)
	results := c.FetchLogs(ctx, sel.Namespace, streams, opts, filter, maxBytes)

	var b strings.Builder
	fmt.Fprintf(&b, "%s %q (selector %s) in namespace %q, context %q: %d pods, %d container logs\n",
		sel.Kind, sel.Name, sel.Selector, sel.Namespace, c.Context, len(sel.Pods), len(streams))
	if len(streams) == 0 {
		b.WriteString("No matching pods or containers.\n")
	}
	for _, r := range results {
		fmt.Fprintf(&b, "\n=== pod/%s [%s]", r.Pod, r.Container)
		switch {
		case r.Err != nil && r.Lines == 0:
			fmt.Fprintf(&b, " ERROR: %v ===\n", r.Err)
			continue
		case r.Err != nil:
			fmt.Fprintf(&b, " (partial, %v)", r.Err)
		}
		fmt.Fprintf(&b, " %d lines", r.Lines)
		if r.Truncated {
			fmt.Fprintf(&b, ", truncated to newest %d bytes", maxBytes)
		}
		b.WriteString(" ===\n")
		if r.Text != "" {
			b.WriteString(r.Text)
			b.WriteByte('\n')
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, nil, nil
}

func (h *handlers) downloadServiceLogs(ctx context.Context, _ *mcp.CallToolRequest, in downloadInput) (*mcp.CallToolResult, downloadOutput, error) {
	opts, err := in.options()
	if err != nil {
		return nil, downloadOutput{}, err
	}
	if in.TailLines < 0 {
		return nil, downloadOutput{}, errors.New("tail_lines must be >= 0")
	}
	opts.TailLines = in.TailLines
	if in.ProjectDir == "" && !isSafeDirName(in.Service) {
		return nil, downloadOutput{}, fmt.Errorf("service %q is not a valid directory name", in.Service)
	}

	c, sel, err := h.selectPods(ctx, in.serviceTarget)
	if err != nil {
		return nil, downloadOutput{}, err
	}
	dir, err := h.downloadDir(in, c.Context)
	if err != nil {
		return nil, downloadOutput{}, err
	}
	files, err := c.DownloadLogs(ctx, sel.Namespace, kube.Streams(sel.Pods, opts), opts, dir)
	if err != nil {
		return nil, downloadOutput{}, err
	}

	out := downloadOutput{
		Context: c.Context, Namespace: sel.Namespace, Kind: sel.Kind, Selector: sel.Selector,
		Dir: dir, Files: files,
	}
	if out.Files == nil {
		out.Files = []kube.FileResult{}
	}
	for _, f := range files {
		if f.Error != "" {
			out.Failed++
		}
	}
	return nil, out, nil
}

func (h *handlers) cluster(in nsTarget) (*kube.Cluster, string, error) {
	c, err := h.cfg.Kube.Cluster(in.Kubeconfig, in.Context)
	if err != nil {
		return nil, "", err
	}
	ns := in.Namespace
	if ns == "" {
		ns = h.cfg.Namespace
	}
	if ns == "" {
		ns = c.Namespace
	}
	return c, ns, nil
}

func (h *handlers) selectPods(ctx context.Context, in serviceTarget) (*kube.Cluster, *kube.Selection, error) {
	if in.Service == "" {
		return nil, nil, errors.New("service is required")
	}
	c, ns, err := h.cluster(in.nsTarget)
	if err != nil {
		return nil, nil, err
	}
	sel, err := c.SelectPods(ctx, ns, in.Service, in.LabelSelector)
	if err != nil {
		return nil, nil, err
	}
	return c, sel, nil
}

func (p logParams) options() (kube.LogOptions, error) {
	opts := kube.LogOptions{
		Container:   p.Container,
		IncludeInit: p.IncludeInit,
		Previous:    p.Previous,
		Timestamps:  p.Timestamps,
	}
	if p.Since != "" {
		d, err := time.ParseDuration(p.Since)
		if err != nil || d <= 0 {
			return opts, fmt.Errorf("invalid since %q: want a positive Go duration like 30m or 2h", p.Since)
		}
		opts.Since = d
	}
	return opts, nil
}

func describePod(p corev1.Pod) podInfo {
	pi := podInfo{
		Name:  p.Name,
		Phase: string(p.Status.Phase),
		Node:  p.Spec.NodeName,
		Age:   time.Since(p.CreationTimestamp.Time).Round(time.Second).String(),
	}
	if p.DeletionTimestamp != nil {
		pi.Phase = "Terminating"
	}
	ready := 0
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		pi.Restarts += cs.RestartCount
	}
	pi.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
	for _, c := range p.Spec.Containers {
		pi.Containers = append(pi.Containers, c.Name)
	}
	for _, c := range p.Spec.InitContainers {
		pi.InitContainers = append(pi.InitContainers, c.Name)
	}
	return pi
}

// downloadDir returns the directory for one download:
// <project_dir>/logs/<env>/<timestamp> when the call names a project, else
// <logs-dir>/<service>/<timestamp>.
func (h *handlers) downloadDir(in downloadInput, kubeContext string) (string, error) {
	stamp := time.Now().Format("20060102-150405")
	if in.ProjectDir == "" {
		return filepath.Join(h.cfg.LogsDir, in.Service, stamp), nil
	}
	if !filepath.IsAbs(in.ProjectDir) {
		return "", fmt.Errorf("project_dir %q must be an absolute path", in.ProjectDir)
	}
	fi, err := os.Stat(in.ProjectDir)
	if err != nil {
		return "", fmt.Errorf("project_dir: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("project_dir %q is not a directory", in.ProjectDir)
	}
	env := h.cfg.Env
	if env == "" {
		env = kubeContext
	}
	return filepath.Join(in.ProjectDir, "logs", sanitizeDirName(env), stamp), nil
}

// sanitizeDirName turns name into a single safe path element.
func sanitizeDirName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, name)
	if !isSafeDirName(name) || strings.Trim(name, ".") == "" {
		return "default"
	}
	return name
}

// isSafeDirName reports whether name can be used as a single path element
// below the logs dir.
func isSafeDirName(name string) bool {
	return name != "" && filepath.IsLocal(name) && filepath.Base(name) == name
}
