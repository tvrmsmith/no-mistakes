package steps

import (
	"context"
	"fmt"
	"path"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// agentInstructionBasenames never qualify for the exemption, whatever
// restart.exempt_paths says. They steer the agents that run in every later
// step, so a commit that edits one must be revalidated, and the glob syntax
// cannot express "*.md except AGENTS.md" anyway.
var agentInstructionBasenames = []string{"AGENTS.md", "CLAUDE.md"}

// restartExemptCommit reports whether a commit's changed paths are all
// documentation, so it carries nothing a validation gate needs to see again.
func restartExemptCommit(paths, exemptPaths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		base := path.Base(p)
		for _, agentBasename := range agentInstructionBasenames {
			if base == agentBasename {
				return false
			}
		}
		exempt := false
		for _, pattern := range exemptPaths {
			if matchIgnorePattern(p, pattern) {
				exempt = true
				break
			}
		}
		if !exempt {
			return false
		}
	}
	return true
}

// commitPathsSinceHead lists the paths a step's own commits changed, as the
// rename-blind NUL-separated diff between sinceSHA and HEAD.
func commitPathsSinceHead(ctx context.Context, workDir, sinceSHA string) ([]string, error) {
	out, err := git.RunRaw(ctx, workDir, "diff", "--name-only", "-z", "--no-renames", sinceSHA+"..HEAD")
	if err != nil {
		return nil, fmt.Errorf("list paths committed since %s: %w", sinceSHA, err)
	}
	return changedPathList(string(out)), nil
}
