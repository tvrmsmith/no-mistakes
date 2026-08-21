package update

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
)

// confirmActiveRunsBeforeUpdate answers whether the update may proceed.
//
// The comparison plan is the currently installed binary's, because the
// incoming binary's layout does not exist yet at this point. That catches a
// legacy or unrecorded plan and a run parked under an older layout; it cannot
// see a layout change shipped by the version being installed.
func (u *updater) confirmActiveRunsBeforeUpdate() error {
	decision, err := u.guardDecision()
	if err != nil {
		return err
	}
	blocking := decision.Blocking
	if len(blocking) == 0 {
		return nil
	}

	u.writeActiveRunWarning(blocking)
	runWord, verb := lifecycle.RunCountWords(len(blocking))
	if u.force {
		fmt.Fprintln(u.stderrWriter(), "FORCE: continuing update and daemon restart despite active pipeline runs")
		return nil
	}

	return fmt.Errorf("refusing update because %d active pipeline %s %s in progress; pass --force to stop/restart the daemon anyway", len(blocking), runWord, verb)
}

// parkedNoticeAtRestart re-reads the guard immediately before the daemon is
// restarted, so the promise names the runs still parked at that moment rather
// than the ones parked before a multi-minute download and replace. A read
// failure promises nothing.
func (u *updater) parkedNoticeAtRestart() string {
	decision, err := u.guardDecision()
	if err != nil {
		return ""
	}
	return decision.ParkedNotice()
}

func (u *updater) guardDecision() (lifecycle.GuardDecision, error) {
	decision, err := lifecycle.Decide(u.paths, steps.AllSteps(), lifecycle.ReplacementBinary)
	if err != nil {
		return lifecycle.GuardDecision{}, fmt.Errorf("check active pipeline runs: %w", err)
	}
	return decision, nil
}

func (u *updater) writeActiveRunWarning(runs []*db.Run) {
	runWord, verb := lifecycle.RunCountWords(len(runs))
	fmt.Fprintf(u.stderrWriter(), "warning: update will restart the daemon while %d active pipeline %s %s in progress\n", len(runs), runWord, verb)
	fmt.Fprint(u.stderrWriter(), lifecycle.RunList(runs))
	fmt.Fprintln(u.stderrWriter(), "continuing can cause these pipelines to fail")
}

func readYes(input io.Reader) bool {
	if input == nil {
		input = os.Stdin
	}
	response, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && response == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(response))
	return answer == "y" || answer == "yes"
}
