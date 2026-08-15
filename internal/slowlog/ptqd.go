package slowlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	PTQDReady            = "ready"
	PTQDUnsupported      = "unsupported"
	PTQDFailed           = "failed"
	DefaultPTQDVersion   = "3.7.1-4"
	DefaultPTQDTimeout   = time.Minute
	DefaultPTQDMaxOutput = int64(16 << 20)
	DefaultPTQDMaxMemory = uint64(512 << 20)
	HardPTQDMaxMemory    = uint64(2 << 30)
)

type Executor interface {
	Run(ctx context.Context, executable string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error
}

type commandExecutor struct {
	limiter        string
	memoryMaxBytes uint64
	cpuMaxSeconds  uint64
}

func (e commandExecutor) Run(ctx context.Context, executable string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	program, limited := e.command(executable, args)
	command := exec.CommandContext(ctx, program, limited...)
	command.Env = env
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return command.Run()
}

func (e commandExecutor) command(executable string, args []string) (string, []string) {
	limited := []string{"--as=" + strconv.FormatUint(e.memoryMaxBytes, 10), "--cpu=" + strconv.FormatUint(e.cpuMaxSeconds, 10), "--nofile=64", "--", executable}
	limited = append(limited, args...)
	return e.limiter, limited
}

type PTQD struct {
	Executable      string
	Executor        Executor
	ExpectedVersion string
	Timeout         time.Duration
	MaxOutputBytes  int64
	MaxMemoryBytes  uint64
}

type PTQDResult struct {
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	Version    string `json:"version,omitempty"`
	Visibility string `json:"visibility"`
	Diagnostic string `json:"diagnostic,omitempty"`
	Report     []byte `json:"-"`
}

func (p PTQD) Run(ctx context.Context, input io.Reader) PTQDResult {
	result := PTQDResult{Status: PTQDFailed, Visibility: "restricted"}
	if p.Executable == "" {
		p.Executable = "pt-query-digest"
	}
	if p.ExpectedVersion == "" {
		p.ExpectedVersion = DefaultPTQDVersion
	}
	if p.Timeout == 0 {
		p.Timeout = DefaultPTQDTimeout
	}
	if p.MaxOutputBytes == 0 {
		p.MaxOutputBytes = DefaultPTQDMaxOutput
	}
	if p.MaxMemoryBytes == 0 {
		p.MaxMemoryBytes = DefaultPTQDMaxMemory
	}
	if p.Timeout < time.Second || p.Timeout > 10*time.Minute || p.MaxOutputBytes < 1 || p.MaxOutputBytes > 64<<20 || p.MaxMemoryBytes < 64<<20 || p.MaxMemoryBytes > HardPTQDMaxMemory {
		result.Code, result.Diagnostic = "invalid-budget", "analyzer resource budget is invalid"
		return result
	}
	if p.Executor == nil {
		executable, findErr := resolveSystemExecutable(p.Executable)
		if findErr != nil {
			result.Status, result.Code, result.Diagnostic = PTQDUnsupported, "tool-unavailable", "pt-query-digest is unavailable"
			return result
		}
		limiter, findErr := resolveSystemExecutable("prlimit")
		if findErr != nil {
			result.Status, result.Code, result.Diagnostic = PTQDUnsupported, "isolation-unavailable", "hard process isolation is unavailable"
			return result
		}
		p.Executable = executable
		cpuSeconds := uint64((p.Timeout + time.Second - 1) / time.Second)
		p.Executor = commandExecutor{limiter: limiter, memoryMaxBytes: p.MaxMemoryBytes, cpuMaxSeconds: cpuSeconds}
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	versionCtx, cancelVersion := context.WithTimeout(ctx, minDuration(p.Timeout, 10*time.Second))
	versionOutput := &boundedBuffer{max: 4096}
	versionErr := p.Executor.Run(versionCtx, p.Executable, []string{"--version"}, env, strings.NewReader(""), versionOutput, &boundedBuffer{max: 4096})
	cancelVersion()
	if versionErr != nil {
		if errors.Is(versionErr, exec.ErrNotFound) {
			result.Status, result.Code, result.Diagnostic = PTQDUnsupported, "tool-unavailable", "pt-query-digest is unavailable"
		} else {
			result.Code, result.Diagnostic = "version-check-failed", "pt-query-digest version check failed"
		}
		return result
	}
	observedVersion := strings.TrimSpace(versionOutput.String())
	if !strings.Contains(observedVersion, p.ExpectedVersion) {
		result.Code, result.Diagnostic = "version-mismatch", "pt-query-digest version does not match the configured version"
		return result
	}
	// Persist the configured pinned token, not the tool's whole banner. A
	// wrapper or compromised executable cannot smuggle paths or credentials
	// into portable metadata through --version output.
	result.Version = p.ExpectedVersion
	runCtx, cancelRun := context.WithTimeout(ctx, p.Timeout)
	defer cancelRun()
	output := &boundedBuffer{max: p.MaxOutputBytes}
	errOutput := &boundedBuffer{max: 4096}
	args := []string{"--limit=20", "--order-by=Query_time:sum", "--report-format=profile"}
	runErr := p.Executor.Run(runCtx, p.Executable, args, env, input, output, errOutput)
	if errors.Is(output.err, errBoundedOutput) {
		result.Code, result.Diagnostic = "output-too-large", "pt-query-digest output exceeded the byte limit"
		return result
	}
	if runErr != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.Code, result.Diagnostic = "timeout", "pt-query-digest exceeded the time limit"
		} else {
			result.Code, result.Diagnostic = "analyzer-failed", "pt-query-digest exited without a usable report"
		}
		return result
	}
	result.Status, result.Report = PTQDReady, append([]byte(nil), output.Bytes()...)
	return result
}

func resolveSystemExecutable(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\r\n\x00") {
		return "", errors.New("slowlog: unsafe executable name")
	}
	for _, directory := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || !singleLink(info) {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("slowlog: executable %s is unavailable", name)
}

func singleLink(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink == 1
}

var errBoundedOutput = errors.New("slowlog: bounded output exceeded")

type boundedBuffer struct {
	bytes.Buffer
	max int64
	err error
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if int64(len(value)) > b.max-int64(b.Len()) {
		b.err = errBoundedOutput
		return 0, b.err
	}
	return b.Buffer.Write(value)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
