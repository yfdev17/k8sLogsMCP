package tools

import (
	"encoding/json"
	"path/filepath"
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

func TestDownloadDir(t *testing.T) {
	project := t.TempDir()
	h := &handlers{cfg: Config{LogsDir: "/srv/logs", Env: "dev"}}

	tests := []struct {
		name    string
		h       *handlers
		in      downloadInput
		want    string // directory above the timestamp
		wantErr bool
	}{
		{"no project", h, downloadInput{serviceTarget: serviceTarget{Service: "svc"}}, "/srv/logs/svc", false},
		{"project with env", h, downloadInput{ProjectDir: project}, filepath.Join(project, "logs", "dev"), false},
		{"project falls back to context", &handlers{}, downloadInput{ProjectDir: project}, filepath.Join(project, "logs", "arn_aws_eks_cluster"), false},
		{"relative project", h, downloadInput{ProjectDir: "rel"}, "", true},
		{"missing project", h, downloadInput{ProjectDir: filepath.Join(project, "nope")}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.h.downloadDir(tt.in, "arn:aws:eks/cluster")
			if (err != nil) != tt.wantErr {
				t.Fatalf("downloadDir error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && filepath.Dir(got) != tt.want {
				t.Errorf("downloadDir = %q, want %q/<timestamp>", got, tt.want)
			}
		})
	}
}
