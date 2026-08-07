package isutools

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ekusiadadus/isutools/internal/runctl"
)

type requiredStartFailure struct{}

func (requiredStartFailure) Name() string { return "required-profile-gate" }

func (requiredStartFailure) CaptureBaseline(context.Context, string, runctl.Epoch) (runctl.SampleResult, error) {
	return runctl.SampleResult{}, errors.New("required opening capture failed")
}

func (requiredStartFailure) CaptureFinal(context.Context, string, runctl.Epoch) (runctl.SampleResult, error) {
	return runctl.SampleResult{}, nil
}

func (requiredStartFailure) Collect(runctl.BaselineHandle, runctl.BaselineHandle) (any, error) {
	return nil, nil
}

func (requiredStartFailure) Release(runctl.BaselineHandle) {}

func TestResetNowDoesNotCaptureProfilesForAbortedRun(t *testing.T) {
	dir := t.TempDir()
	core := newTestMeasurement(t, map[string]string{
		envDataDir:     dir,
		envHeapProfile: "1",
	})
	if err := core.ctrl.RegisterBaseline(runctl.Registration{
		Name: "required-profile-gate", Required: true,
	}, requiredStartFailure{}); err != nil {
		t.Fatalf("RegisterBaseline: %v", err)
	}

	result, err := core.resetNow(context.Background(), runctl.StartRunOptions{
		Preempt: true,
		Reason:  "test",
		Trigger: "test",
	})
	if err != nil {
		t.Fatalf("resetNow: %v", err)
	}
	if result.State != runctl.StateAborted || result.Validity != runctl.ValidityInvalid {
		t.Fatalf("result = %s/%s, want aborted/invalid", result.State, result.Validity)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("aborted ResetNow published profile artifacts: %v", names)
	}
}
