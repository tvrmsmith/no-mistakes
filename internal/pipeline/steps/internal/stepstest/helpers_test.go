package stepstest

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// TestNewTestContext_SetsCoverageDirOutsideWorkDir verifies every step test
// gets a real, run-scoped coverage directory without opting in, the same way
// EvidenceDir is already populated, and that it lives outside WorkDir so a
// step under test cannot accidentally commit a coverage profile as part of
// its own worktree changes.
func TestNewTestContext_SetsCoverageDirOutsideWorkDir(t *testing.T) {
	workDir, baseSHA, headSHA := SetupGitRepo(t)
	sctx := NewTestContext(t, nil, workDir, baseSHA, headSHA, config.Commands{})

	if sctx.CoverageDir == "" {
		t.Fatal("expected CoverageDir to be set, got empty string")
	}
	if strings.HasPrefix(sctx.CoverageDir, sctx.WorkDir) {
		t.Errorf("CoverageDir %q must not be inside WorkDir %q", sctx.CoverageDir, sctx.WorkDir)
	}
}
