package kube

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// maxParallel bounds concurrent log streams against the API server.
const maxParallel = 8

// LogOptions selects which containers to read and how much of their logs.
type LogOptions struct {
	// Container limits reading to one container name; empty means all.
	Container string
	// IncludeInit also reads init containers.
	IncludeInit bool
	// TailLines keeps the last N lines per container; 0 means everything.
	TailLines int64
	// Since keeps lines newer than this duration; 0 means no limit.
	Since time.Duration
	// Previous reads the previous (crashed) instance of each container.
	Previous   bool
	Timestamps bool
}

// Stream identifies one container log.
type Stream struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
}

// LogResult is the in-memory log of one container.
type LogResult struct {
	Stream
	Text      string
	Lines     int
	Truncated bool
	Err       error
}

// FileResult is one container log written to disk.
type FileResult struct {
	Stream
	Path  string `json:"path,omitempty"`
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
}

// Streams lists the container logs to read for pods.
func Streams(pods []corev1.Pod, opts LogOptions) []Stream {
	var out []Stream
	for _, p := range pods {
		var containers []corev1.Container
		if opts.IncludeInit {
			containers = append(containers, p.Spec.InitContainers...)
		}
		containers = append(containers, p.Spec.Containers...)
		for _, c := range containers {
			if opts.Container != "" && c.Name != opts.Container {
				continue
			}
			out = append(out, Stream{Pod: p.Name, Container: c.Name})
		}
	}
	return out
}

// FetchLogs reads logs into memory. Only lines matching filter (when not nil)
// are kept, and at most maxBytes of the newest of them per container.
func (c *Cluster) FetchLogs(ctx context.Context, ns string, streams []Stream, opts LogOptions, filter *regexp.Regexp, maxBytes int) []LogResult {
	results := make([]LogResult, len(streams))
	forEach(streams, func(i int, s Stream) {
		results[i] = c.fetchOne(ctx, ns, s, opts, filter, maxBytes)
	})
	return results
}

func (c *Cluster) fetchOne(ctx context.Context, ns string, s Stream, opts LogOptions, filter *regexp.Regexp, maxBytes int) LogResult {
	res := LogResult{Stream: s}
	rc, err := c.openLog(ctx, ns, s, opts)
	if err != nil {
		res.Err = err
		return res
	}
	defer func() { _ = rc.Close() }()

	var (
		lines []string
		size  int
	)
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if filter != nil && !filter.MatchString(line) {
			continue
		}
		lines = append(lines, line)
		size += len(line) + 1
		for maxBytes > 0 && size > maxBytes && len(lines) > 1 {
			size -= len(lines[0]) + 1
			lines = lines[1:]
			res.Truncated = true
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = fmt.Errorf("read log: %w", err)
	}
	res.Lines = len(lines)
	res.Text = strings.Join(lines, "\n")
	return res
}

// DownloadLogs streams every log to <dir>/<pod>_<container>[.previous].log.
func (c *Cluster) DownloadLogs(ctx context.Context, ns string, streams []Stream, opts LogOptions, dir string) ([]FileResult, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create logs dir: %w", err)
	}
	suffix := ".log"
	if opts.Previous {
		suffix = ".previous.log"
	}

	results := make([]FileResult, len(streams))
	forEach(streams, func(i int, s Stream) {
		res := FileResult{Stream: s}
		path := filepath.Join(dir, s.Pod+"_"+s.Container+suffix)
		n, err := c.downloadOne(ctx, ns, s, opts, path)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Path, res.Bytes = path, n
		}
		results[i] = res
	})
	return results, nil
}

func (c *Cluster) downloadOne(ctx context.Context, ns string, s Stream, opts LogOptions, path string) (n int64, err error) {
	rc, err := c.openLog(ctx, ns, s, opts)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()

	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close file: %w", cerr)
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()

	n, err = io.Copy(f, rc)
	if err != nil {
		return n, fmt.Errorf("write log: %w", err)
	}
	return n, nil
}

func (c *Cluster) openLog(ctx context.Context, ns string, s Stream, opts LogOptions) (io.ReadCloser, error) {
	po := &corev1.PodLogOptions{
		Container:  s.Container,
		Previous:   opts.Previous,
		Timestamps: opts.Timestamps,
	}
	if opts.TailLines > 0 {
		po.TailLines = &opts.TailLines
	}
	if opts.Since > 0 {
		secs := int64(opts.Since.Seconds())
		po.SinceSeconds = &secs
	}
	rc, err := c.Client.CoreV1().Pods(ns).GetLogs(s.Pod, po).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("open log of %s/%s: %w", s.Pod, s.Container, err)
	}
	return rc, nil
}

// forEach calls fn for every stream with at most maxParallel in flight. fn
// must only write to its own index of any shared slice.
func forEach(streams []Stream, fn func(i int, s Stream)) {
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, s := range streams {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			fn(i, s)
		})
	}
	wg.Wait()
}
