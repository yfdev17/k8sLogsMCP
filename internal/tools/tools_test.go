package tools

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"k8s-logs-mcp/internal/kube"
)

func TestToolSchemas(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	Register(server, Config{Kube: kube.NewManager("", ""), LogsDir: t.TempDir()})

	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}

	tool, ok := byName["get_service_logs"]
	if !ok {
		t.Fatalf("get_service_logs not registered; got %v", len(res.Tools))
	}
	raw, _ := json.Marshal(tool.InputSchema)
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"kubeconfig", "context", "namespace", "service", "label_selector", "since", "filter", "tail_lines"} {
		if _, ok := schema.Properties[p]; !ok {
			t.Errorf("get_service_logs schema lacks property %q", p)
		}
	}
	if !slices.Equal(schema.Required, []string{"service"}) {
		t.Errorf("required = %v, want [service]", schema.Required)
	}
}

func TestIsSafeDirName(t *testing.T) {
	for name, want := range map[string]bool{
		"api":      true,
		"api-v2.1": true,
		"":         false,
		"..":       false,
		"../etc":   false,
		"a/b":      false,
		"/abs":     false,
	} {
		if got := isSafeDirName(name); got != want {
			t.Errorf("isSafeDirName(%q) = %v, want %v", name, got, want)
		}
	}
}
