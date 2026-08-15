package slowlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type fakeExecutor struct {
	calls [][]string
	envs  [][]string
	run   func(args []string, stdin io.Reader, stdout, stderr io.Writer) error
}

func (f *fakeExecutor) Run(_ context.Context, executable string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	f.calls = append(f.calls, append([]string{executable}, args...))
	f.envs = append(f.envs, append([]string(nil), env...))
	return f.run(args, stdin, stdout, stderr)
}

func TestPTQDUsesFixedArgumentsAndRestrictedOutput(t *testing.T) {
	fake := &fakeExecutor{run: func(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if len(args) == 1 && args[0] == "--version" {
			_, _ = io.WriteString(stdout, "pt-query-digest 3.7.1-4 wrapper password=never-persist\n")
			return nil
		}
		body, _ := io.ReadAll(stdin)
		if string(body) != sampleSlowLog {
			t.Fatal("input mismatch")
		}
		_, _ = io.WriteString(stdout, "restricted report with SQL samples")
		_, _ = io.WriteString(stderr, "DSN=password secret")
		return nil
	}}
	runner := PTQD{Executable: "/usr/bin/pt-query-digest", Executor: fake, ExpectedVersion: "3.7.1-4", Timeout: time.Second, MaxOutputBytes: 1024}
	result := runner.Run(context.Background(), strings.NewReader(sampleSlowLog))
	if result.Status != PTQDReady || string(result.Report) != "restricted report with SQL samples" || result.Visibility != "restricted" || result.Version != "3.7.1-4" {
		t.Fatalf("result=%+v", result)
	}
	if len(fake.calls) != 2 || strings.Join(fake.calls[1], " ") != "/usr/bin/pt-query-digest --limit=20 --order-by=Query_time:sum --report-format=profile" {
		t.Fatalf("calls=%v", fake.calls)
	}
	for _, env := range fake.envs {
		joined := strings.Join(env, " ")
		if strings.Contains(joined, "PASSWORD") || strings.Contains(joined, "HOME=") || !strings.Contains(joined, "LC_ALL=C") {
			t.Fatalf("unsafe env=%v", env)
		}
	}
	if strings.Contains(result.Diagnostic, "password") || strings.Contains(result.Diagnostic, "secret") {
		t.Fatalf("stderr leaked: %+v", result)
	}
}

func TestPTQDFailsClosedOnVersionOutputAndTimeout(t *testing.T) {
	tests := []struct {
		name string
		run  func([]string, io.Reader, io.Writer, io.Writer) error
		code string
	}{
		{"version", func(args []string, _ io.Reader, stdout, _ io.Writer) error {
			_, _ = io.WriteString(stdout, "pt-query-digest 3.6.0")
			return nil
		}, "version-mismatch"},
		{"output", func(args []string, _ io.Reader, stdout, _ io.Writer) error {
			if len(args) == 1 {
				_, _ = io.WriteString(stdout, "pt-query-digest 3.7.1-4")
				return nil
			}
			_, err := io.Copy(stdout, bytes.NewReader(bytes.Repeat([]byte("x"), 100)))
			return err
		}, "output-too-large"},
		{"crash", func(args []string, _ io.Reader, stdout, _ io.Writer) error {
			if len(args) == 1 {
				_, _ = io.WriteString(stdout, "pt-query-digest 3.7.1-4")
				return nil
			}
			return errors.New("contains password=secret")
		}, "analyzer-failed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeExecutor{run: tc.run}
			result := (PTQD{Executable: "pt-query-digest", Executor: fake, ExpectedVersion: "3.7.1-4", Timeout: time.Second, MaxOutputBytes: 32}).Run(context.Background(), strings.NewReader(sampleSlowLog))
			if result.Status == PTQDReady || result.Code != tc.code || strings.Contains(result.Diagnostic, "secret") {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestCommandExecutorUsesHardPrlimitWithoutShell(t *testing.T) {
	executor := commandExecutor{limiter: "/usr/bin/prlimit", memoryMaxBytes: 512 << 20, cpuMaxSeconds: 60}
	program, args := executor.command("/usr/bin/pt-query-digest", []string{"--limit=20"})
	if program != "/usr/bin/prlimit" || strings.Join(args, " ") != "--as=536870912 --cpu=60 --nofile=64 -- /usr/bin/pt-query-digest --limit=20" {
		t.Fatalf("program=%q args=%v", program, args)
	}
}

func TestPTQDRejectsMemoryBudgetOutsideHardRange(t *testing.T) {
	fake := &fakeExecutor{run: func([]string, io.Reader, io.Writer, io.Writer) error { return nil }}
	for _, memory := range []uint64{1, HardPTQDMaxMemory + 1} {
		result := (PTQD{Executor: fake, Timeout: time.Second, MaxOutputBytes: 32, MaxMemoryBytes: memory}).Run(context.Background(), strings.NewReader(sampleSlowLog))
		if result.Code != "invalid-budget" || result.Status == PTQDReady || len(fake.calls) != 0 {
			t.Fatalf("memory=%d result=%+v calls=%v", memory, result, fake.calls)
		}
	}
}
