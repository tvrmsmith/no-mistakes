package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/spf13/cobra"
)

// TestAxiHome_SkewedDaemonReadsAsRunning holds the axi home view to the same
// reading `status` gets: a daemon on another protocol version holds the socket
// and answers, so home must not report it stopped and send an agent to start a
// daemon that is already there. The skew is the actionable fact, so home
// carries it and its remedy beside the running state.
func TestAxiHome_SkewedDaemonReadsAsRunning(t *testing.T) {
	skew := &ipc.VersionMismatchError{Local: 1, Remote: 0, RemoteRole: ipc.RoleDaemon}

	tests := []struct {
		name        string
		alive       bool
		err         error
		wantIn      []string
		wantMissing []string
	}{
		{
			name:        "healthy daemon",
			alive:       true,
			wantIn:      []string{"daemon: running"},
			wantMissing: []string{"daemon_version_skew"},
		},
		{
			name:        "absent daemon",
			wantIn:      []string{"daemon: stopped"},
			wantMissing: []string{"daemon_version_skew"},
		},
		{
			name:   "skewed daemon",
			err:    skew,
			wantIn: []string{"daemon: running", "daemon_version_skew:", "cli v1", "daemon v0", "daemon restart"},
		},
		{
			name:        "unrelated read failure stays stopped",
			alive:       false,
			err:         errStubDaemonRead,
			wantIn:      []string{"daemon: stopped"},
			wantMissing: []string{"daemon_version_skew"},
		},
	}

	fingerprints := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubDaemonIsRunning(t, tt.alive, tt.err)
			out, fingerprint := runAxiHomeForTest(t)
			fingerprints[tt.name] = fingerprint
			for _, want := range tt.wantIn {
				if !strings.Contains(out, want) {
					t.Errorf("axi home missing %q in:\n%s", want, out)
				}
			}
			for _, forbidden := range tt.wantMissing {
				if strings.Contains(out, forbidden) {
					t.Errorf("axi home should not contain %q in:\n%s", forbidden, out)
				}
			}
		})
	}

	// The daemon state feeds the read-surface telemetry fingerprint, which
	// only re-emits when the fingerprint changes. A skew resolved by a daemon
	// restart is a state change an operator cares about, so it must not hide
	// behind the same fingerprint a healthy daemon produces.
	if fingerprints["skewed daemon"] == fingerprints["healthy daemon"] {
		t.Errorf("skewed and healthy daemon share fingerprint %q", fingerprints["healthy daemon"])
	}
}

var errStubDaemonRead = &stubError{"connect to daemon socket: no such file"}

type stubError struct{ msg string }

func (e *stubError) Error() string { return e.msg }

func stubDaemonIsRunning(t *testing.T, alive bool, err error) {
	t.Helper()
	prev := daemonIsRunningFn
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return alive, err }
	t.Cleanup(func() { daemonIsRunningFn = prev })
}

// runAxiHomeForTest renders the home view against an isolated NM_HOME and a
// throwaway repository, never the operator's real root.
func runAxiHomeForTest(t *testing.T) (output, fingerprint string) {
	t.Helper()
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	rawRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		rawRoot = repoDir
	}
	chdir(t, rawRoot)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	if _, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main"); err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	fingerprint, err = runAxiHome(cmd)
	if err != nil {
		t.Fatalf("axi home: %v\n%s", err, out.String())
	}
	return out.String(), fingerprint
}
