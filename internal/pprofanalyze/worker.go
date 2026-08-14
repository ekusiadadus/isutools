package pprofanalyze

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	pprofprofile "github.com/google/pprof/profile"

	"github.com/ekusiadadus/isutools/internal/profilecapture"
	"github.com/ekusiadadus/isutools/internal/profilemodel"
)

const (
	workerSubcommand       = "__isutools_pprof_worker"
	workerControlFD        = 3
	workerReadyFD          = 4
	workerRequestFD        = 5
	workerResultFD         = 6
	workerFirstProfileFD   = 7
	workerMaxRequestBytes  = 256 << 10
	workerMaxResultBytes   = profilemodel.MaxAnalysisBodyBytes
	workerMaxReadyBytes    = 4 << 10
	defaultWorkerTimeout   = 30 * time.Second
	defaultMemoryMax       = profilemodel.MaxWorkerMemoryBytes
	defaultAddressSpaceMax = profilemodel.MaxWorkerAddressBytes
)

var ErrHardIsolationUnavailable = errors.New("pprofanalyze: hard worker isolation is unavailable")

type WorkerOptions struct {
	CgroupRoot           string
	MemoryMaxBytes       uint64
	AddressSpaceMaxBytes uint64
	Timeout              time.Duration
}

type WorkerJob struct {
	Mode       string                          `json:"mode"`
	TopN       int                             `json:"top_n"`
	Dictionary *profilecapture.LabelDictionary `json:"dictionary,omitempty"`
	Profiles   []*os.File                      `json:"-"`
}

type WorkerResult struct {
	Isolation           profilemodel.WorkerIsolation  `json:"isolation"`
	Summaries           []profilemodel.ProfileSummary `json:"summaries,omitempty"`
	Flame               *profilemodel.FlameGraph      `json:"flame,omitempty"`
	ForeignProfileLabel bool                          `json:"foreign_profile_label,omitempty"`
	ErrorCode           string                        `json:"error_code,omitempty"`
	ErrorMessage        string                        `json:"error_message,omitempty"`
}

type workerRequest struct {
	Mode       string                          `json:"mode"`
	TopN       int                             `json:"top_n"`
	Profiles   int                             `json:"profiles"`
	Dictionary *profilecapture.LabelDictionary `json:"dictionary,omitempty"`
}

type workerReady struct {
	PID       int                          `json:"pid"`
	Isolation profilemodel.WorkerIsolation `json:"isolation"`
	Error     string                       `json:"error,omitempty"`
}

// IsWorkerInvocation recognizes the fixed hidden argv used by LaunchWorker.
// A CLI must call this before parsing public subcommands, then exit with the
// status returned by RunInheritedWorker.
func IsWorkerInvocation(args []string) bool {
	return len(args) == 2 && args[1] == workerSubcommand
}

// LaunchWorker runs the current trusted analyzer executable. Profile paths are
// never placed in argv or environment; only already-open read-only descriptors
// are inherited. A platform without the full hard bootstrap fails before the
// child is started and before any profile byte is read.
func LaunchWorker(ctx context.Context, executable string, job WorkerJob, options WorkerOptions) (WorkerResult, error) {
	request, err := validateWorkerJob(job)
	if err != nil {
		return WorkerResult{}, err
	}
	options, err = normalizedWorkerOptions(options)
	if err != nil {
		return WorkerResult{}, err
	}
	state, err := newWorkerPlatformState(options)
	if err != nil {
		return WorkerResult{}, fmt.Errorf("%w: %v", ErrHardIsolationUnavailable, err)
	}
	return launchWorkerWithState(ctx, executable, job, options, request, state)
}

func launchWorkerWithState(ctx context.Context, executable string, job WorkerJob, options WorkerOptions, request workerRequest, state workerPlatformState) (out WorkerResult, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, state.Close()) }()
	requestBody, err := json.Marshal(request)
	if err != nil {
		return WorkerResult{}, err
	}

	controlR, controlW, err := os.Pipe()
	if err != nil {
		return WorkerResult{}, err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		_ = controlR.Close()
		_ = controlW.Close()
		return WorkerResult{}, err
	}
	requestR, requestW, err := os.Pipe()
	if err != nil {
		_ = controlR.Close()
		_ = controlW.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		return WorkerResult{}, err
	}
	resultR, resultW, err := os.Pipe()
	if err != nil {
		_ = controlR.Close()
		_ = controlW.Close()
		_ = readyR.Close()
		_ = readyW.Close()
		_ = requestR.Close()
		_ = requestW.Close()
		return WorkerResult{}, err
	}
	parentFiles := []*os.File{controlW, readyR, requestW, resultR}
	childFiles := []*os.File{controlR, readyW, requestR, resultW}
	closeAll := func(files []*os.File) {
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
	}
	defer closeAll(parentFiles)
	defer closeAll(childFiles)

	cmd := exec.Command(executable, workerSubcommand)
	cmd.Env = append(os.Environ(), workerEnvironment(options)...)
	cmd.ExtraFiles = append(childFiles, job.Profiles...)
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	state.Configure(cmd)
	if err := cmd.Start(); err != nil {
		return WorkerResult{}, fmt.Errorf("pprofanalyze: start worker: %w", err)
	}
	closeAll(childFiles)

	deadline := options.Timeout
	if deadline == 0 {
		deadline = defaultWorkerTimeout
	}
	workerCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	type readyResult struct {
		ready workerReady
		err   error
	}
	readyCh := make(chan readyResult, 1)
	go func() {
		body, readErr := io.ReadAll(io.LimitReader(readyR, workerMaxReadyBytes+1))
		if readErr != nil {
			readyCh <- readyResult{err: readErr}
			return
		}
		if len(body) > workerMaxReadyBytes {
			readyCh <- readyResult{err: errors.New("worker ready message exceeds limit")}
			return
		}
		var ready workerReady
		decodeErr := decodeStrictJSON(body, &ready)
		readyCh <- readyResult{ready: ready, err: decodeErr}
	}()
	var boot workerReady
	select {
	case <-workerCtx.Done():
		_ = state.Kill(cmd.Process)
		_ = cmd.Wait()
		return WorkerResult{}, fmt.Errorf("pprofanalyze: worker bootstrap: %w", workerCtx.Err())
	case received := <-readyCh:
		if received.err != nil || received.ready.Error != "" {
			_ = state.Kill(cmd.Process)
			_ = cmd.Wait()
			if received.err != nil {
				return WorkerResult{}, fmt.Errorf("%w: invalid bootstrap message: %v", ErrHardIsolationUnavailable, received.err)
			}
			return WorkerResult{}, fmt.Errorf("%w: %s", ErrHardIsolationUnavailable, received.ready.Error)
		}
		boot = received.ready
	}
	if boot.PID != cmd.Process.Pid {
		_ = state.Kill(cmd.Process)
		_ = cmd.Wait()
		return WorkerResult{}, fmt.Errorf("%w: worker PID mismatch", ErrHardIsolationUnavailable)
	}
	isolation, err := state.VerifyStopped(workerCtx, cmd.Process, boot.Isolation)
	if err != nil {
		_ = state.Kill(cmd.Process)
		_ = cmd.Wait()
		return WorkerResult{}, fmt.Errorf("%w: %v", ErrHardIsolationUnavailable, err)
	}
	if err := state.Resume(cmd.Process); err != nil {
		_ = state.Kill(cmd.Process)
		_ = cmd.Wait()
		return WorkerResult{}, fmt.Errorf("pprofanalyze: resume worker: %w", err)
	}
	if _, err := io.WriteString(controlW, "START\n"); err != nil {
		_ = state.Kill(cmd.Process)
		_ = cmd.Wait()
		return WorkerResult{}, fmt.Errorf("pprofanalyze: release worker gate: %w", err)
	}
	_ = controlW.Close()
	controlW = nil
	requestWriteCh := startWorkerRequestWrite(requestW, requestBody)
	requestW = nil

	type outputResult struct {
		body []byte
		err  error
	}
	outputCh := make(chan outputResult, 1)
	go func() {
		body, readErr := io.ReadAll(io.LimitReader(resultR, workerMaxResultBytes+1))
		outputCh <- outputResult{body: body, err: readErr}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var output outputResult
	for {
		select {
		case <-workerCtx.Done():
			_ = state.Kill(cmd.Process)
			<-waitCh
			return WorkerResult{}, fmt.Errorf("pprofanalyze: worker execution: %w", workerCtx.Err())
		case writeErr := <-requestWriteCh:
			requestWriteCh = nil
			if writeErr != nil {
				_ = state.Kill(cmd.Process)
				<-waitCh
				return WorkerResult{}, fmt.Errorf("pprofanalyze: send worker request: %w", writeErr)
			}
		case waitErr := <-waitCh:
			output = <-outputCh
			if waitErr != nil {
				return WorkerResult{}, fmt.Errorf("pprofanalyze: worker exited: %v: %s", waitErr, stderr.String())
			}
			goto workerComplete
		}
	}

workerComplete:
	if output.err != nil {
		return WorkerResult{}, fmt.Errorf("pprofanalyze: read worker result: %w", output.err)
	}
	if len(output.body) > workerMaxResultBytes {
		return WorkerResult{}, errors.New("pprofanalyze: worker result exceeds limit")
	}
	var result WorkerResult
	if err := decodeStrictJSON(output.body, &result); err != nil {
		return WorkerResult{}, fmt.Errorf("pprofanalyze: invalid worker result: %w", err)
	}
	if result.Isolation != isolation {
		return WorkerResult{}, errors.New("pprofanalyze: worker isolation result does not match parent proof")
	}
	return result, nil
}

// startWorkerRequestWrite keeps a request larger than the kernel pipe buffer
// from blocking the parent's verified-stop bootstrap. The worker reads only
// after the parent has observed the stop and sent START.
func startWorkerRequestWrite(writer io.WriteCloser, body []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		for len(body) > 0 {
			written, err := writer.Write(body)
			if err != nil {
				done <- errors.Join(err, writer.Close())
				return
			}
			if written == 0 {
				done <- errors.Join(io.ErrShortWrite, writer.Close())
				return
			}
			body = body[written:]
		}
		done <- writer.Close()
	}()
	return done
}

// RunInheritedWorker executes only inside the hidden worker invocation.
func RunInheritedWorker() int {
	readyFile := os.NewFile(workerReadyFD, "worker-ready")
	resultFile := os.NewFile(workerResultFD, "worker-result")
	if readyFile == nil || resultFile == nil {
		return 4
	}
	return runInheritedWorkerWithIO(readyFile, resultFile, childPrepareIsolation, childSelfStop,
		func() io.ReadCloser { return os.NewFile(workerControlFD, "worker-control") },
		func() io.ReadCloser { return os.NewFile(workerRequestFD, "worker-request") },
		func(index int) *os.File { return os.NewFile(uintptr(workerFirstProfileFD+index), "profile-input") },
	)
}

func runInheritedWorkerWithIO(readyFile io.WriteCloser, resultFile io.WriteCloser, prepare func() (profilemodel.WorkerIsolation, error), stop func() error, openControl func() io.ReadCloser, openRequest func() io.ReadCloser, openProfile func(int) *os.File) int {
	defer func() { _ = readyFile.Close() }()
	defer func() { _ = resultFile.Close() }()
	isolation, err := prepare()
	ready := workerReady{PID: os.Getpid(), Isolation: isolation}
	if err != nil {
		ready.Error = boundedError(err)
		_ = json.NewEncoder(readyFile).Encode(ready)
		return 4
	}
	if err := json.NewEncoder(readyFile).Encode(ready); err != nil {
		return 4
	}
	_ = readyFile.Close()
	control := openControl()
	requestFile := openRequest()
	if control == nil || requestFile == nil {
		return 4
	}
	return runWorkerAfterReady(control, requestFile, resultFile, isolation, stop, openProfile)
}

func runWorkerAfterReady(control io.ReadCloser, requestFile io.ReadCloser, resultFile io.Writer, isolation profilemodel.WorkerIsolation, stop func() error, openProfile func(int) *os.File) int {
	if stop == nil || openProfile == nil || stop() != nil {
		return 4
	}
	command, err := bufio.NewReader(io.LimitReader(control, 16)).ReadString('\n')
	_ = control.Close()
	if err != nil || command != "START\n" {
		return 4
	}
	isolation.StoppedVerified = true
	if isolation.Mode == profilemodel.IsolationLinuxCgroupV2 {
		isolation.HardLimitVerified = true
		isolation.MembershipVerified = true
	}
	requestBody, err := io.ReadAll(io.LimitReader(requestFile, workerMaxRequestBytes+1))
	_ = requestFile.Close()
	if err != nil || len(requestBody) > workerMaxRequestBytes {
		return writeWorkerFailure(resultFile, isolation, profilemodel.DiagnosticProfileInvalid, "invalid bounded worker request")
	}
	var request workerRequest
	if err := decodeStrictJSON(requestBody, &request); err != nil {
		return writeWorkerFailure(resultFile, isolation, profilemodel.DiagnosticProfileInvalid, "invalid worker request")
	}
	result := executeWorkerRequest(request, isolation, newVerifiedIsolationProof(), openProfile)
	if err := json.NewEncoder(resultFile).Encode(result); err != nil {
		return 4
	}
	return 0
}

func executeWorkerRequest(request workerRequest, isolation profilemodel.WorkerIsolation, proof IsolationProof, openProfile func(int) *os.File) WorkerResult {
	result := WorkerResult{Isolation: isolation}
	if (request.Mode != profilemodel.ProfileModeInterval && request.Mode != profilemodel.ProfileModeCumulativeDelta) ||
		request.TopN <= 0 || request.TopN > profilemodel.MaxTopNodes ||
		(request.Mode == profilemodel.ProfileModeInterval && request.Profiles != 1) ||
		(request.Mode == profilemodel.ProfileModeCumulativeDelta && request.Profiles != 2) {
		result.ErrorCode, result.ErrorMessage = profilemodel.DiagnosticProfileInvalid, "invalid worker operation"
		return result
	}
	profiles := make([]*DecodedProfile, 0, request.Profiles)
	for index := 0; index < request.Profiles; index++ {
		file := openProfile(index)
		if file == nil {
			result.ErrorCode, result.ErrorMessage = profilemodel.DiagnosticProfileMissing, "profile descriptor is unavailable"
			return result
		}
		decoded, err := Decode(file, DefaultLimits(), proof)
		_ = file.Close()
		if err != nil {
			result.ErrorCode, result.ErrorMessage = workerDiagnostic(err), boundedError(err)
			return result
		}
		profiles = append(profiles, decoded)
	}
	var err error
	if request.Mode == profilemodel.ProfileModeInterval {
		result.Summaries, err = AnalyzeInterval(profiles[0], request.TopN)
		if err == nil {
			flame, flameErr := BuildFlame(profiles[0].Profile)
			result.Flame, err = &flame, flameErr
		}
		if err == nil && request.Dictionary != nil {
			for index := range result.Summaries {
				labels, foreign, labelErr := AggregateTrustedLabels(profiles[0].Profile, index, *request.Dictionary)
				if labelErr != nil {
					err = labelErr
					break
				}
				result.Summaries[index].Labels = labels
				result.ForeignProfileLabel = result.ForeignProfileLabel || foreign
			}
		}
	} else {
		result.Summaries, err = AnalyzeDelta(profiles[0], profiles[1], request.TopN)
		if err == nil {
			var delta *pprofprofile.Profile
			delta, err = DeltaProfile(profiles[0], profiles[1], request.TopN)
			if err == nil {
				flame, flameErr := BuildFlame(delta)
				result.Flame, err = &flame, flameErr
			}
		}
	}
	if err != nil {
		result.Summaries = nil
		result.Flame = nil
		result.ErrorCode, result.ErrorMessage = workerDiagnostic(err), boundedError(err)
	}
	return result
}

func validateWorkerJob(job WorkerJob) (workerRequest, error) {
	request := workerRequest{Mode: job.Mode, TopN: job.TopN, Profiles: len(job.Profiles), Dictionary: job.Dictionary}
	if (job.Mode != profilemodel.ProfileModeInterval && job.Mode != profilemodel.ProfileModeCumulativeDelta) ||
		job.TopN <= 0 || job.TopN > profilemodel.MaxTopNodes ||
		(job.Mode == profilemodel.ProfileModeInterval && len(job.Profiles) != 1) ||
		(job.Mode == profilemodel.ProfileModeCumulativeDelta && len(job.Profiles) != 2) {
		return workerRequest{}, errors.New("pprofanalyze: invalid worker job")
	}
	for _, file := range job.Profiles {
		if file == nil {
			return workerRequest{}, errors.New("pprofanalyze: profile descriptor is required")
		}
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return workerRequest{}, errors.New("pprofanalyze: profile descriptor must be a regular file")
		}
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > workerMaxRequestBytes {
		return workerRequest{}, errors.New("pprofanalyze: worker request exceeds limit")
	}
	return request, nil
}

func normalizedWorkerOptions(options WorkerOptions) (WorkerOptions, error) {
	if options.MemoryMaxBytes == 0 {
		options.MemoryMaxBytes = defaultMemoryMax
	}
	if options.AddressSpaceMaxBytes == 0 {
		options.AddressSpaceMaxBytes = defaultAddressSpaceMax
	}
	if options.Timeout == 0 {
		options.Timeout = defaultWorkerTimeout
	}
	if options.MemoryMaxBytes > defaultMemoryMax || options.MemoryMaxBytes < 128<<20 ||
		options.AddressSpaceMaxBytes > defaultAddressSpaceMax || options.AddressSpaceMaxBytes < options.MemoryMaxBytes ||
		options.Timeout <= 0 || options.Timeout > 5*time.Minute {
		return WorkerOptions{}, errors.New("pprofanalyze: invalid worker limits")
	}
	return options, nil
}

func workerEnvironment(options WorkerOptions) []string {
	return []string{
		"ISUTOOLS_PPROF_WORKER_MEMORY_MAX=" + strconv.FormatUint(options.MemoryMaxBytes, 10),
		"ISUTOOLS_PPROF_WORKER_AS_MAX=" + strconv.FormatUint(options.AddressSpaceMaxBytes, 10),
	}
}

func workerLimitFromEnv(name string, maximum uint64) (uint64, error) {
	value, err := strconv.ParseUint(os.Getenv(name), 10, 64)
	if err != nil || value == 0 || value > maximum {
		return 0, errors.New("invalid worker limit")
	}
	return value, nil
}

func decodeStrictJSON(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func workerDiagnostic(err error) string {
	message := err.Error()
	for _, code := range []string{
		profilemodel.DiagnosticSampleValueOverflow,
		profilemodel.DiagnosticNegativeIntervalSample,
		profilemodel.DiagnosticSampleTypeIncompatible,
	} {
		if strings.Contains(message, code) {
			return code
		}
	}
	if strings.Contains(message, "exceeds") {
		return profilemodel.DiagnosticProfileTooLarge
	}
	return profilemodel.DiagnosticProfileInvalid
}

func writeWorkerFailure(writer io.Writer, isolation profilemodel.WorkerIsolation, code, message string) int {
	if json.NewEncoder(writer).Encode(WorkerResult{Isolation: isolation, ErrorCode: code, ErrorMessage: message}) != nil {
		return 4
	}
	return 0
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, err.Error())
	if len(message) > 1024 {
		message = message[:1024]
	}
	return message
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	want := len(p)
	remaining := 4096 - b.Len()
	if remaining > 0 {
		if remaining < len(p) {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return want, nil
}

type workerPlatformState interface {
	Configure(*exec.Cmd)
	VerifyStopped(context.Context, *os.Process, profilemodel.WorkerIsolation) (profilemodel.WorkerIsolation, error)
	Resume(*os.Process) error
	Kill(*os.Process) error
	Close() error
}
