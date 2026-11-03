// Command k8s-logs-mcp is an MCP server that reads logs from every pod of a
// Kubernetes service using a kubeconfig file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"k8s-logs-mcp/internal/kube"
	"k8s-logs-mcp/internal/tools"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("k8s-logs-mcp failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		kubeconfig = flag.String("kubeconfig", "", "kubeconfig file (default $KUBECONFIG or ~/.kube/config)")
		kubeCtx    = flag.String("context", "", "kubeconfig context (default current-context)")
		namespace  = flag.String("namespace", "", "default namespace (default: the context namespace)")
		logsDir    = flag.String("logs-dir", "logs", "directory for downloaded logs when a call names no project")
		env        = flag.String("env", "", "environment name for project log paths <project>/logs/<env>/ (default: the context name)")
		transport  = flag.String("transport", "stdio", "transport: stdio or http")
		addr       = flag.String("addr", "127.0.0.1:8080", "listen address for the http transport")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println(version)
		return nil
	}

	// stdout carries the MCP protocol on stdio, so logs go to stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	dir, err := filepath.Abs(*logsDir)
	if err != nil {
		return fmt.Errorf("resolve logs dir: %w", err)
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "k8s-logs-mcp",
		Title:   "Kubernetes service logs",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Reads logs from all pods of a Kubernetes service. Typical flow: list_contexts -> " +
			"list_namespaces -> list_services -> get_service_logs (quick look, filtered) or " +
			"download_service_logs (complete logs to files).",
	})
	tools.Register(server, tools.Config{
		Kube:      kube.NewManager(*kubeconfig, *kubeCtx),
		LogsDir:   dir,
		Env:       *env,
		Namespace: *namespace,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch *transport {
	case "stdio":
		return server.Run(ctx, &mcp.StdioTransport{})
	case "http":
		return serveHTTP(ctx, server, *addr)
	default:
		return fmt.Errorf("unknown transport %q", *transport)
	}
}

func serveHTTP(ctx context.Context, server *mcp.Server, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	slog.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
