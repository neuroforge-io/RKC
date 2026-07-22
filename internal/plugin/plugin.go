package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/repository-knowledge-compiler/rkc/internal/model"
)

type FileRef struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Language string `json:"language"`
	SHA256   string `json:"sha256"`
}

type Request struct {
	SchemaVersion string    `json:"schema_version"`
	SnapshotID    string    `json:"snapshot_id"`
	Root          string    `json:"root"`
	Files         []FileRef `json:"files"`
}

type PythonOptions struct {
	Interpreter    string
	Script         string
	Timeout        time.Duration
	MaxOutputBytes int64
	MaxStderrBytes int64
}

func RunPython(ctx context.Context, request Request, opts PythonOptions) (model.Fragment, error) {
	if len(request.Files) == 0 {
		return model.Fragment{}, nil
	}
	if opts.Interpreter == "" {
		opts.Interpreter = "python3"
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	if opts.Script == "" {
		return model.Fragment{}, fmt.Errorf("python plugin script is required")
	}
	absScript, err := filepath.Abs(opts.Script)
	if err != nil {
		return model.Fragment{}, fmt.Errorf("resolve plugin path: %w", err)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return model.Fragment{}, fmt.Errorf("encode plugin request: %w", err)
	}

	pluginCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cmd := exec.CommandContext(pluginCtx, opts.Interpreter, absScript)
	cmd.Stdin = bytes.NewReader(payload)
	stdout := newLimitedBuffer(opts.MaxOutputBytes, 64*1024*1024)
	stderr := newLimitedBuffer(opts.MaxStderrBytes, 2*1024*1024)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if pluginCtx.Err() != nil {
			return model.Fragment{}, fmt.Errorf("python plugin timed out after %s", opts.Timeout)
		}
		return model.Fragment{}, fmt.Errorf("python plugin failed: %w: %s", err, stderr.String())
	}
	if stdout.Truncated() {
		return model.Fragment{}, fmt.Errorf("python plugin output exceeded %d bytes", stdout.Limit())
	}
	if stderr.Truncated() {
		return model.Fragment{}, fmt.Errorf("python plugin stderr exceeded %d bytes", stderr.Limit())
	}
	var fragment model.Fragment
	if err := json.Unmarshal(stdout.Bytes(), &fragment); err != nil {
		return model.Fragment{}, fmt.Errorf("decode python plugin response: %w; stderr=%s", err, stderr.String())
	}
	return fragment, nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func newLimitedBuffer(configured, fallback int64) *limitedBuffer {
	if configured <= 0 {
		configured = fallback
	}
	return &limitedBuffer{limit: configured}
}
func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.written += int64(len(p))
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		chunk := p
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
		}
		_, _ = b.buffer.Write(chunk)
	}
	if b.written > b.limit {
		b.truncated = true
	}
	return len(p), nil
}
func (b *limitedBuffer) Bytes() []byte   { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string  { return b.buffer.String() }
func (b *limitedBuffer) Truncated() bool { return b.truncated }
func (b *limitedBuffer) Limit() int64    { return b.limit }
