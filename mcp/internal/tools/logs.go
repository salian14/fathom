package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
)

type logInput struct {
	Name         string `json:"name" jsonschema:"Name of the pod. Required."`
	Namespace    string `json:"namespace,omitempty" jsonschema:"Namespace of the pod. Omit for the default namespace."`
	Container    string `json:"container,omitempty" jsonschema:"Container to read from. Required only when the pod has more than one container."`
	TailLines    int64  `json:"tailLines,omitempty" jsonschema:"How many lines from the end of the log to read. Bounded by the server's own limit."`
	Previous     bool   `json:"previous,omitempty" jsonschema:"Read the log of the previous, terminated instance of the container. Use this after a restart or crash loop, when the current log is empty or has started over."`
	SinceSeconds int64  `json:"sinceSeconds,omitempty" jsonschema:"Only return lines newer than this many seconds ago."`
	Timestamps   bool   `json:"timestamps,omitempty" jsonschema:"Prefix every line with its RFC3339 timestamp."`
}

func (s *Server) handleLog(ctx context.Context, _ *mcp.CallToolRequest, in logInput) (*mcp.CallToolResult, any, error) {
	if in.Name == "" {
		return nil, nil, fmt.Errorf("name is required; use k8s_get with resource \"pods\" to find the pod name")
	}
	if err := ValidateName(in.Name); err != nil {
		return nil, nil, err
	}
	if err := ValidateNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}
	if err := ValidateContainer(in.Container); err != nil {
		return nil, nil, err
	}
	if in.SinceSeconds < 0 {
		return nil, nil, fmt.Errorf("sinceSeconds must not be negative, got %d", in.SinceSeconds)
	}

	mapping, err := s.resolveAllowed("pods")
	if err != nil {
		return nil, nil, err
	}

	namespace, err := s.namespaceFor(mapping, in.Namespace, false)
	if err != nil {
		return nil, nil, err
	}

	tail := in.TailLines
	if tail <= 0 {
		tail = s.cfg.LogTailLines
	}
	if tail > s.cfg.LogMaxTailLines {
		tail = s.cfg.LogMaxTailLines
	}

	ceiling := int64(s.cfg.LogMaxBytes) * 16

	opts := &corev1.PodLogOptions{
		Container:  in.Container,
		Previous:   in.Previous,
		Timestamps: in.Timestamps,
		TailLines:  &tail,
		LimitBytes: &ceiling,
	}
	if in.SinceSeconds > 0 {
		opts.SinceSeconds = &in.SinceSeconds
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	stream, err := s.client.Clientset.CoreV1().
		Pods(namespace).
		GetLogs(in.Name, opts).
		Stream(ctx)
	if err != nil {
		return nil, nil, logError(err, namespace, in.Name, in.Container, in.Previous)
	}
	defer stream.Close()

	data, dropped, err := readTail(stream, s.cfg.LogMaxBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("reading logs for pod %q in namespace %q: %w", in.Name, namespace, err)
	}

	if len(data) == 0 {
		hint := ""
		if !in.Previous {
			hint = " If the container has restarted, try again with previous=true."
		}
		return text(fmt.Sprintf("No log output for pod %q%s in namespace %q.%s\n",
			in.Name, containerSuffix(in.Container), namespace, hint)), nil, nil
	}

	out := string(data)
	if dropped {
		out = fmt.Sprintf("[truncated: showing the last %d bytes of the log]\n", len(data)) + out
	}
	return text(out), nil, nil
}

func readTail(r io.Reader, keep int) (data []byte, dropped bool, err error) {
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)

	for {
		n, readErr := r.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if buf.Len() > 2*keep {
				b := buf.Bytes()
				trimmed := append([]byte(nil), b[len(b)-keep:]...)
				buf.Reset()
				buf.Write(trimmed)
				dropped = true
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false, readErr
		}
	}

	out := buf.Bytes()
	if len(out) > keep {
		out = out[len(out)-keep:]
		dropped = true
	}
	if dropped {
		if i := bytes.IndexByte(out, '\n'); i >= 0 && i < len(out)-1 {
			out = out[i+1:]
		}
	}
	return out, dropped, nil
}

func logError(err error, namespace, pod, container string, previous bool) error {
	base := apiError(err, "pods", namespace, pod)
	switch {
	case container == "":
		return fmt.Errorf("%w. If the pod has multiple containers, pass one in 'container'; "+
			"k8s_describe on the pod lists them", base)
	case previous:
		return fmt.Errorf("%w. A previous log exists only after the container has restarted "+
			"at least once", base)
	default:
		return base
	}
}

func containerSuffix(container string) string {
	if container == "" {
		return ""
	}
	return fmt.Sprintf(" (container %q)", container)
}
