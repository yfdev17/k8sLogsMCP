// Package kube wraps client-go: it loads kubeconfig files (the same files
// kubectl and OpenLens use), resolves a service name to its pods and reads
// their container logs.
package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // OIDC and other auth providers used in kubeconfigs.
	"k8s.io/client-go/tools/clientcmd"
)

// Manager builds Kubernetes clients from kubeconfig files and caches them per
// (kubeconfig, context) pair.
type Manager struct {
	defaultPath    string
	defaultContext string

	mu       sync.Mutex
	clusters map[clusterKey]*Cluster
}

type clusterKey struct{ path, context string }

// Cluster is a client bound to one kubeconfig context.
type Cluster struct {
	Client kubernetes.Interface
	// Context is the kubeconfig context name the client was built for.
	Context string
	// Namespace is the default namespace of that context.
	Namespace string
}

// ContextInfo describes one context of a kubeconfig file.
type ContextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Server    string `json:"server,omitempty"`
	User      string `json:"user"`
	Namespace string `json:"namespace,omitempty"`
	Current   bool   `json:"current"`
}

// NewManager returns a Manager. An empty path falls back to $KUBECONFIG and
// then ~/.kube/config; an empty context uses the file's current-context.
func NewManager(path, context string) *Manager {
	return &Manager{
		defaultPath:    expandHome(path),
		defaultContext: context,
		clusters:       make(map[clusterKey]*Cluster),
	}
}

// Cluster returns a client for the given kubeconfig path and context. Empty
// values fall back to the Manager defaults.
func (m *Manager) Cluster(path, context string) (*Cluster, error) {
	path, context = m.resolve(path, context)
	key := clusterKey{path, context}

	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clusters[key]; ok {
		return c, nil
	}

	cc := clientConfig(path, context)
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if context == "" {
		context = raw.CurrentContext
	}
	rest, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("build client config for context %q: %w", context, err)
	}
	rest.QPS, rest.Burst = 50, 100

	client, err := kubernetes.NewForConfig(rest)
	if err != nil {
		return nil, fmt.Errorf("create client for context %q: %w", context, err)
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, fmt.Errorf("read namespace of context %q: %w", context, err)
	}

	c := &Cluster{Client: client, Context: context, Namespace: ns}
	m.clusters[key] = c
	return c, nil
}

// Contexts lists the contexts of a kubeconfig file, sorted by name.
func (m *Manager) Contexts(path string) ([]ContextInfo, error) {
	path, _ = m.resolve(path, "")
	raw, err := clientConfig(path, "").RawConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	if len(raw.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig %q has no contexts", path)
	}

	out := make([]ContextInfo, 0, len(raw.Contexts))
	for name, c := range raw.Contexts {
		info := ContextInfo{
			Name:      name,
			Cluster:   c.Cluster,
			User:      c.AuthInfo,
			Namespace: c.Namespace,
			Current:   name == raw.CurrentContext,
		}
		if cl, ok := raw.Clusters[c.Cluster]; ok {
			info.Server = cl.Server
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// resolve applies Manager defaults. The default context only applies to the
// default kubeconfig: another file is unlikely to contain it.
func (m *Manager) resolve(path, context string) (string, string) {
	if path == "" {
		path = m.defaultPath
		if context == "" {
			context = m.defaultContext
		}
	}
	return expandHome(path), context
}

func clientConfig(path, context string) clientcmd.ClientConfig {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: context})
}

func expandHome(path string) string {
	rest, ok := strings.CutPrefix(path, "~")
	if !ok || (rest != "" && rest[0] != '/') {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, rest)
}
