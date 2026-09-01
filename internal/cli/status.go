package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show status of the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackReadSurface("status", nil, func() (string, string, error) {
				p, d, err := openResources()
				if err != nil {
					return "", "", err
				}
				defer d.Close()

				w := cmd.OutOrStdout()

				// Look up repo from current directory.
				repo, err := findRepo(d)
				if err != nil {
					fmt.Fprintln(w, err)
					return "uninitialized", "error", nil
				}

				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  repo:"), repo.WorkingPath)
				remoteURL := repo.UpstreamURL
				if repo.ForkURL != "" {
					remoteURL = safeurl.Redact(remoteURL)
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("remote:"), remoteURL)
				if repo.ForkURL != "" {
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  fork:"), safeurl.Redact(repo.ForkURL))
				}
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("  gate:"), p.RepoDir(repo.ID))

				// Check daemon status.
				alive, runningErr := daemonIsRunningFn(p)
				daemonState, daemonText := daemonStatusRow(alive, runningErr)
				fmt.Fprintf(w, "  %s  %s\n", sDim.Render("daemon:"), daemonText)

				// Check for active run.
				activeRun, err := d.GetActiveRun(repo.ID, "")
				if err != nil {
					return "", "", fmt.Errorf("check active run: %w", err)
				}
				fingerprint := statusFingerprint(repo.ID, daemonState, activeRun)
				if syncState := (&branchsync.Service{DB: d, Repo: repo, WorkDir: "."}).InspectCached(cmd.Context()); relevantCachedSyncState(syncState) {
					fmt.Fprintf(w, "\n  %s  %s\n", sDim.Render("local branch:"), humanSyncSummary(syncState))
				}
				if activeRun != nil {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sCyan.Render("Active run"))
					sha := activeRun.HeadSHA[:minLen(len(activeRun.HeadSHA), 8)]
					ts := time.Unix(activeRun.CreatedAt, 0).Format(time.DateTime)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("     id:"), activeRun.ID)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" branch:"), activeRun.Branch)
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render(" status:"), runStatusStyle(activeRun.Status))
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("   head:"), sDim.Render(sha))
					fmt.Fprintf(w, "  %s  %s\n", sDim.Render("started:"), sDim.Render(ts))
				} else {
					fmt.Fprintf(w, "\n  %s\n", sDim.Render("no active run"))
				}

				return fingerprint, "success", nil
			})
		},
	}
}

// daemonStatusRow renders the daemon row and the state its fingerprint keys
// on. A version-skewed daemon reads as RUNNING: daemon.IsRunning answers the
// narrower "is a COMPATIBLE daemon there" and returns false with a mismatch
// error, but that daemon holds the socket and answers, so reporting it stopped
// hides a live process during exactly the window the handshake exists to make
// legible. The skew is the actionable fact, so the row carries it and its
// remedy instead of a bare "running". Any other read failure stays stopped:
// only a mismatch proves something answered.
func daemonStatusRow(alive bool, err error) (state, text string) {
	var mismatch *ipc.VersionMismatchError
	if errors.As(err, &mismatch) {
		return "running", fmt.Sprintf("%s running - %s. %s",
			sYellow.Render("●"), mismatch.Summary(), mismatch.Remedy())
	}
	if alive {
		return "running", fmt.Sprintf("%s running", sGreen.Render("●"))
	}
	return "stopped", fmt.Sprintf("%s stopped", sDim.Render("○"))
}

func statusFingerprint(repoID, daemonState string, activeRun *db.Run) string {
	if activeRun == nil {
		return repoID + "|" + daemonState + "|idle"
	}
	return fmt.Sprintf("%s|%s|%s:%s:%s:%s", repoID, daemonState, activeRun.ID, activeRun.Branch, activeRun.Status, activeRun.HeadSHA)
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
