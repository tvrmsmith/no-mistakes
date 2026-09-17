package citest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// TestEveryShortTimeoutTestPinsTheClock guards this package against the flake
// class pinCIMonitorClock undoes.
func TestEveryShortTimeoutTestPinsTheClock(t *testing.T) {
	t.Parallel()
	stepstest.AssertShortCITimeoutTestsPinTheClock(t, ".")
}

// pinCIMonitorClock wires stepstest.PinnedCIMonitorClock into step; that
// function owns the rationale.
func pinCIMonitorClock(step *steps.CIStep) {
	now, baseBranchTip := stepstest.PinnedCIMonitorClock()
	step.SetNow(now).SetBaseBranchTip(baseBranchTip)
}

// failOnExtraPoll is the waitForNextPoll for a test whose step must resolve on
// its first poll. Under a frozen clock nothing else would stop the loop, so a
// regression has to surface as this error rather than as a hang.
func failOnExtraPoll(context.Context, time.Duration) error {
	return errors.New("CI monitor polled again instead of resolving on its first poll")
}

func assertCIRestartsValidation(t *testing.T, outcome *pipeline.StepOutcome, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("CI repair returned error: %v", err)
	}
	if outcome == nil || outcome.RestartFrom != pipeline.RestartBoundary {
		t.Fatalf("CI repair outcome = %#v, want restart from %s", outcome, pipeline.RestartBoundary)
	}
}
