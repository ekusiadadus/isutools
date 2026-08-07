package pprofanalyze

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

type nopReadCloser struct{ io.Reader }

func (nopReadCloser) Close() error { return nil }

type bufferWriteCloser struct{ *bytes.Buffer }

func (bufferWriteCloser) Close() error { return nil }

func TestMain(m *testing.M) {
	if os.Getenv("ISUTOOLS_PPROF_PARENT_PROTOCOL_TEST") == "1" && IsWorkerInvocation(os.Args) {
		os.Exit(runParentProtocolTestWorker())
	}
	if IsWorkerInvocation(os.Args) {
		os.Exit(RunInheritedWorker())
	}
	os.Exit(m.Run())
}

func TestWorkerInvocationIsExact(t *testing.T) {
	t.Parallel()
	if !IsWorkerInvocation([]string{"isutools-pprof", workerSubcommand}) {
		t.Fatal("fixed worker invocation was not recognized")
	}
	for _, args := range [][]string{{"isutools-pprof"}, {"isutools-pprof", workerSubcommand, "extra"}, {"isutools-pprof", "worker"}} {
		if IsWorkerInvocation(args) {
			t.Fatalf("accepted public or ambiguous argv %q", args)
		}
	}
}

func TestWorkerRequestWriteDoesNotBlockBootstrap(t *testing.T) {
	t.Parallel()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body := bytes.Repeat([]byte("x"), workerMaxRequestBytes)
	done := startWorkerRequestWrite(writer, body)
	select {
	case err := <-done:
		t.Fatalf("large request completed before the worker could read it: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	readDone := make(chan error, 1)
	go func() {
		got, readErr := io.ReadAll(reader)
		if readErr == nil && !bytes.Equal(got, body) {
			readErr = errors.New("worker request body changed in transit")
		}
		readDone <- readErr
	}()
	if err := <-done; err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read request: %v", err)
	}
}

func TestWorkerLimitsCannotBeRaisedAboveReviewedCeilings(t *testing.T) {
	t.Parallel()
	for _, options := range []WorkerOptions{
		{MemoryMaxBytes: defaultMemoryMax + 1},
		{AddressSpaceMaxBytes: defaultAddressSpaceMax + 1},
		{MemoryMaxBytes: 64 << 20},
		{MemoryMaxBytes: defaultMemoryMax, AddressSpaceMaxBytes: defaultMemoryMax - 1},
		{Timeout: 6 * time.Minute},
	} {
		if _, err := normalizedWorkerOptions(options); err == nil {
			t.Fatalf("accepted invalid limits %#v", options)
		}
	}
}

func TestLaunchWorkerDarwinVerifiesOrFailsBeforeProfileRead(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin RLIMIT integration; Linux requires a delegated cgroup fixture")
	}
	body := encodedProfile(t, syntheticProfile(7), true)
	path := t.TempDir() + "/profile.pprof"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = profile.Close() }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result, err := LaunchWorker(context.Background(), executable, WorkerJob{
		Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile},
	}, WorkerOptions{Timeout: 10 * time.Second})
	if err != nil {
		if !errors.Is(err, ErrHardIsolationUnavailable) {
			t.Fatalf("LaunchWorker: %v", err)
		}
		offset, seekErr := profile.Seek(0, 1)
		if seekErr != nil || offset != 0 {
			t.Fatalf("unsupported Darwin bootstrap consumed profile: offset=%d err=%v", offset, seekErr)
		}
		return
	}
	if result.ErrorCode != "" || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 7 ||
		result.Isolation.Mode != profilemodel.IsolationDarwinRLIMIT || !result.Isolation.HardLimitVerified || !result.Isolation.StoppedVerified {
		t.Fatalf("worker result = %#v", result)
	}
}

func TestLaunchWorkerFailsBeforeReadingWithoutPlatformPrimitive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux fail-closed configuration test")
	}
	profile, err := os.CreateTemp(t.TempDir(), "profile")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = profile.Close() }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	_, err = LaunchWorker(context.Background(), executable, WorkerJob{
		Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{profile},
	}, WorkerOptions{})
	if !errors.Is(err, ErrHardIsolationUnavailable) {
		t.Fatalf("LaunchWorker = %v, want fail-closed hard isolation error", err)
	}
	offset, seekErr := profile.Seek(0, 1)
	if seekErr != nil || offset != 0 {
		t.Fatalf("profile descriptor was consumed: offset=%d err=%v", offset, seekErr)
	}
}

func TestRunWorkerAfterReadyDecodesOnlyAfterStopAndStart(t *testing.T) {
	body := encodedProfile(t, syntheticProfile(9), true)
	path := t.TempDir() + "/profile.pprof"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(workerRequest{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	var output bytes.Buffer
	code := runWorkerAfterReady(
		nopReadCloser{strings.NewReader("START\n")}, nopReadCloser{bytes.NewReader(requestBody)}, &output,
		profilemodel.WorkerIsolation{Mode: profilemodel.IsolationDarwinRLIMIT, Bootstrap: profilemodel.BootstrapRLIMITSIGSTOP, HardLimitVerified: true},
		func() error { stopped = true; return nil },
		func(int) *os.File {
			if !stopped {
				t.Fatal("profile opened before stopped bootstrap")
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				t.Fatal(openErr)
			}
			return file
		},
	)
	if code != 0 {
		t.Fatalf("worker exit = %d", code)
	}
	var result WorkerResult
	if err := decodeStrictJSON(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 9 || !result.Isolation.StoppedVerified {
		t.Fatalf("result = %#v", result)
	}
}

func TestInheritedWorkerProtocolWritesReadyThenResult(t *testing.T) {
	body := encodedProfile(t, syntheticProfile(11), true)
	path := t.TempDir() + "/profile.pprof"
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	requestBody, err := json.Marshal(workerRequest{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	var ready, output bytes.Buffer
	code := runInheritedWorkerWithIO(
		bufferWriteCloser{&ready}, bufferWriteCloser{&output},
		func() (profilemodel.WorkerIsolation, error) {
			value := isolationForUnitTest()
			value.StoppedVerified = false
			return value, nil
		},
		func() error { return nil },
		func() io.ReadCloser { return nopReadCloser{strings.NewReader("START\n")} },
		func() io.ReadCloser { return nopReadCloser{bytes.NewReader(requestBody)} },
		func(int) *os.File { file, _ := os.Open(path); return file },
	)
	if code != 0 || !strings.Contains(ready.String(), `"pid":`) {
		t.Fatalf("exit=%d ready=%s result=%s", code, ready.String(), output.String())
	}
	var result WorkerResult
	if err := decodeStrictJSON(output.Bytes(), &result); err != nil || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 11 {
		t.Fatalf("result=%#v err=%v", result, err)
	}

	ready.Reset()
	output.Reset()
	code = runInheritedWorkerWithIO(
		bufferWriteCloser{&ready}, bufferWriteCloser{&output},
		func() (profilemodel.WorkerIsolation, error) {
			return profilemodel.WorkerIsolation{}, errors.New("no hard limit")
		},
		func() error { t.Fatal("stop called after failed prepare"); return nil },
		func() io.ReadCloser { return nil }, func() io.ReadCloser { return nil }, func(int) *os.File { return nil },
	)
	if code != 4 || !strings.Contains(ready.String(), "no hard limit") || output.Len() != 0 {
		t.Fatalf("failed prepare exit=%d ready=%s output=%s", code, ready.String(), output.String())
	}
}

func TestRunWorkerAfterReadyRejectsProtocolBeforeOpeningProfile(t *testing.T) {
	opened := false
	openProfile := func(int) *os.File { opened = true; return nil }
	isolation := profilemodel.WorkerIsolation{Mode: profilemodel.IsolationDarwinRLIMIT, HardLimitVerified: true}
	for _, test := range []struct {
		name    string
		control string
		request string
		stop    func() error
		want    int
	}{
		{name: "stop", control: "START\n", request: `{}`, stop: func() error { return errors.New("stop failed") }, want: 4},
		{name: "control", control: "NO\n", request: `{}`, stop: func() error { return nil }, want: 4},
		{name: "request", control: "START\n", request: `{`, stop: func() error { return nil }, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			opened = false
			var output bytes.Buffer
			got := runWorkerAfterReady(nopReadCloser{strings.NewReader(test.control)}, nopReadCloser{strings.NewReader(test.request)}, &output, isolation, test.stop, openProfile)
			if got != test.want || opened {
				t.Fatalf("exit=%d opened=%v output=%s", got, opened, output.String())
			}
		})
	}
}

func TestWorkerHelperBoundsAndDiagnostics(t *testing.T) {
	t.Setenv("ISUTOOLS_PPROF_TEST_LIMIT", "12")
	if value, err := workerLimitFromEnv("ISUTOOLS_PPROF_TEST_LIMIT", 12); err != nil || value != 12 {
		t.Fatalf("limit = %d err=%v", value, err)
	}
	t.Setenv("ISUTOOLS_PPROF_TEST_LIMIT", "13")
	if _, err := workerLimitFromEnv("ISUTOOLS_PPROF_TEST_LIMIT", 12); err == nil {
		t.Fatal("accepted worker limit above maximum")
	}
	for err, want := range map[error]string{
		errors.New("sample-value-overflow"):      profilemodel.DiagnosticSampleValueOverflow,
		errors.New("negative-interval-sample"):   profilemodel.DiagnosticNegativeIntervalSample,
		errors.New("sample-type-incompatible"):   profilemodel.DiagnosticSampleTypeIncompatible,
		errors.New("expanded profile exceeds 1"): profilemodel.DiagnosticProfileTooLarge,
		errors.New("malformed"):                  profilemodel.DiagnosticProfileInvalid,
	} {
		if got := workerDiagnostic(err); got != want {
			t.Errorf("workerDiagnostic(%v) = %q, want %q", err, got, want)
		}
	}
	message := boundedError(errors.New(strings.Repeat("x", 1100) + "\n"))
	if len(message) != 1024 || strings.Contains(message, "\n") {
		t.Fatalf("bounded error length/control = %d %q", len(message), message[len(message)-4:])
	}
	var stderr boundedBuffer
	payload := bytes.Repeat([]byte("x"), 5000)
	if n, err := stderr.Write(payload); err != nil || n != len(payload) || stderr.Len() != 4096 {
		t.Fatalf("bounded stderr n=%d len=%d err=%v", n, stderr.Len(), err)
	}
	if n, err := stderr.Write([]byte("more")); err != nil || n != 4 || stderr.Len() != 4096 {
		t.Fatalf("full bounded stderr n=%d len=%d err=%v", n, stderr.Len(), err)
	}
	var failure bytes.Buffer
	if code := writeWorkerFailure(&failure, isolationForUnitTest(), profilemodel.DiagnosticProfileInvalid, "bad"); code != 0 || !strings.Contains(failure.String(), "profile-invalid") {
		t.Fatalf("writeWorkerFailure code=%d body=%s", code, failure.String())
	}
	if boundedError(nil) != "" {
		t.Fatal("nil error did not produce an empty bounded message")
	}
	if code := writeWorkerFailure(errorWriter{}, isolationForUnitTest(), profilemodel.DiagnosticProfileInvalid, "bad"); code != 4 {
		t.Fatalf("failed result writer exit = %d", code)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestExecuteWorkerRequestRejectsBadOperationsAndValues(t *testing.T) {
	isolation := isolationForUnitTest()
	result := executeWorkerRequest(workerRequest{Mode: "bad", TopN: 10}, isolation, newVerifiedIsolationProof(), func(int) *os.File { return nil })
	if result.ErrorCode != profilemodel.DiagnosticProfileInvalid {
		t.Fatalf("bad operation = %#v", result)
	}
	profile := validProfile(math.MinInt64)
	path := t.TempDir() + "/overflow.pprof"
	if err := os.WriteFile(path, encodedProfile(t, profile, false), 0o600); err != nil {
		t.Fatal(err)
	}
	result = executeWorkerRequest(workerRequest{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: 1}, isolation, newVerifiedIsolationProof(), func(int) *os.File {
		file, _ := os.Open(path)
		return file
	})
	if result.ErrorCode != profilemodel.DiagnosticSampleValueOverflow || len(result.Summaries) != 0 {
		t.Fatalf("overflow result = %#v", result)
	}
}

func TestExecuteWorkerRequestDeltaAndMissingDescriptor(t *testing.T) {
	isolation := isolationForUnitTest()
	dir := t.TempDir()
	paths := []string{dir + "/open.pprof", dir + "/close.pprof"}
	for index, value := range []int64{4, 9} {
		if err := os.WriteFile(paths[index], encodedProfile(t, syntheticProfile(value), true), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	result := executeWorkerRequest(
		workerRequest{Mode: profilemodel.ProfileModeCumulativeDelta, TopN: 10, Profiles: 2},
		isolation, newVerifiedIsolationProof(), func(index int) *os.File { file, _ := os.Open(paths[index]); return file },
	)
	if result.ErrorCode != "" || len(result.Summaries) != 1 || result.Summaries[0].NetTotal != 5 || result.Summaries[0].DenominatorMode != profilemodel.DenominatorAbsoluteAddress {
		t.Fatalf("delta result = %#v", result)
	}
	missing := executeWorkerRequest(
		workerRequest{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: 1},
		isolation, newVerifiedIsolationProof(), func(int) *os.File { return nil },
	)
	if missing.ErrorCode != profilemodel.DiagnosticProfileMissing {
		t.Fatalf("missing descriptor = %#v", missing)
	}
}

func TestExecuteWorkerRequestRestoresTrustedLabels(t *testing.T) {
	scope := testLabelScope(t)
	dictionary := scope.Dictionary("run", 1)
	profile := syntheticProfile(3)
	profile.Sample[0].Label = map[string][]string{
		profilecapture.PrivateCaptureLabel: {dictionary.CaptureID},
		profilecapture.PrivateTupleLabel:   {dictionary.Tuples[0].TupleID},
	}
	path := t.TempDir() + "/labels.pprof"
	if err := os.WriteFile(path, encodedProfile(t, profile, true), 0o600); err != nil {
		t.Fatal(err)
	}
	result := executeWorkerRequest(
		workerRequest{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: 1, Dictionary: &dictionary},
		isolationForUnitTest(), newVerifiedIsolationProof(), func(int) *os.File { file, _ := os.Open(path); return file },
	)
	if result.ErrorCode != "" || len(result.Summaries) != 1 || len(result.Summaries[0].Labels) == 0 || result.ForeignProfileLabel {
		t.Fatalf("labeled result = %#v", result)
	}
}

func TestValidateWorkerJobRejectsUnsafeInputs(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "profile")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for _, job := range []WorkerJob{
		{Mode: "unknown", TopN: 10, Profiles: []*os.File{file}},
		{Mode: profilemodel.ProfileModeInterval, TopN: 0, Profiles: []*os.File{file}},
		{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: nil},
		{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{nil}},
		{Mode: profilemodel.ProfileModeCumulativeDelta, TopN: 10, Profiles: []*os.File{file}},
	} {
		if _, err := validateWorkerJob(job); err == nil {
			t.Fatalf("accepted unsafe job %#v", job)
		}
	}
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	if _, err := validateWorkerJob(WorkerJob{Mode: profilemodel.ProfileModeInterval, TopN: 10, Profiles: []*os.File{directory}}); err == nil {
		t.Fatal("accepted directory as profile descriptor")
	}
}

func isolationForUnitTest() profilemodel.WorkerIsolation {
	return profilemodel.WorkerIsolation{
		Mode: profilemodel.IsolationDarwinRLIMIT, Bootstrap: profilemodel.BootstrapRLIMITSIGSTOP,
		AddressSpaceMaxBytes: defaultAddressSpaceMax, HardLimitVerified: true, StoppedVerified: true,
	}
}
