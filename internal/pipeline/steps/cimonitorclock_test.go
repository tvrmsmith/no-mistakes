package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// pinCIMonitorClock wires stepstest.PinnedCIMonitorClock into step; that
// function owns the rationale.
func pinCIMonitorClock(step *CIStep) {
	now, baseBranchTip := stepstest.PinnedCIMonitorClock()
	step.SetNow(now).SetBaseBranchTip(baseBranchTip)
}

// TestEveryShortTimeoutTestPinsTheClock guards this package against the flake
// class pinCIMonitorClock undoes.
func TestEveryShortTimeoutTestPinsTheClock(t *testing.T) {
	t.Parallel()
	stepstest.AssertShortCITimeoutTestsPinTheClock(t, ".")
}
