package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func init() {
	if os.Getenv("NM_FAKE_BIN") == "1" {
		name := filepath.Base(os.Args[0])
		if ext := filepath.Ext(name); ext != "" {
			name = strings.TrimSuffix(name, ext)
		}
		switch name {
		case "git":
			if len(os.Args) > 1 && os.Args[1] == "--version" {
				// The version line is the whole answer the caller reads, so a
				// failed write exits nonzero rather than claiming success.
				if _, err := fmt.Fprintln(os.Stdout, "git version 9.9.9"); err != nil {
					os.Exit(1)
				}
				os.Exit(0)
			}
			os.Exit(1)
		case "gh", "claude":
			os.Exit(0)
		default:
			os.Exit(1)
		}
	}
	if os.Getenv("NM_HOOK_HELPER") == "1" {
		if err := newRootCmd().Execute(); err != nil {
			// Last resort: the helper process exits nonzero straight after,
			// and a failed write to stderr has nowhere left to report itself.
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("NM_DAEMON_HELPER_PROCESS") == "bootstrap-sink" {
		root, ok := explicitDaemonLogSinkRootFromArgs(os.Args[1:])
		if !ok {
			os.Exit(1)
		}
		_ = os.Setenv("NM_HOME", root)
		if err := daemon.RunBootstrapLogSink(); err != nil {
			// Last resort: the helper process exits nonzero straight after,
			// and a failed write to stderr has nowhere left to report itself.
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv("NM_TEST_START_DAEMON") != "1" {
		return
	}
	if root, ok := explicitDaemonRunRootFromArgs(os.Args[1:]); ok && root != "" {
		_ = os.Setenv("NM_HOME", root)
	} else if os.Getenv("NM_DAEMON") != "1" {
		return
	}
	if err := daemon.Run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestMain(m *testing.M) {
	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, "nm-cli-test-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create test NM_HOME: %v\n", err)
		os.Exit(1)
	}
	home, err := os.MkdirTemp(base, "nm-cli-home-")
	if err != nil {
		_ = os.RemoveAll(root)
		_, _ = fmt.Fprintf(os.Stderr, "create test HOME: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("NM_HOME", root)
	_ = os.Setenv("HOME", home)
	// The fixtures here clone, commit, and push for real, so an ambient
	// ~/.gitconfig (commit.gpgsign against a locked signing agent, core.hooksPath,
	// gpg.format) or a harness-injected GIT_CONFIG_* decides whether a fixture
	// commit succeeds. A test that needs injected config re-sets it with
	// t.Setenv. internal/eval/main_test.go isolates the same way.
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_ = os.Unsetenv("GIT_CONFIG_COUNT")
	_ = os.Setenv("NO_MISTAKES_TELEMETRY", "off")
	_ = os.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")

	code := m.Run()

	_ = daemon.Stop(paths.WithRoot(root))
	_ = os.RemoveAll(root)
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func explicitDaemonRunRootFromArgs(args []string) (string, bool) {
	if len(args) < 2 || args[0] != "daemon" || args[1] != "run" {
		return "", false
	}
	if len(args) == 2 {
		return "", true
	}
	if len(args) == 3 {
		if value, ok := strings.CutPrefix(args[2], "--root="); ok {
			return value, true
		}
		return "", false
	}
	if len(args) == 4 && args[2] == "--root" {
		return args[3], true
	}
	return "", false
}

func explicitDaemonLogSinkRootFromArgs(args []string) (string, bool) {
	if len(args) != 4 || args[0] != "daemon" || args[1] != "log-sink" || args[2] != "--root" || args[3] == "" {
		return "", false
	}
	return args[3], true
}

// setupTestRepo creates a git repo with an origin remote in a temp dir and
// sets NM_HOME to an isolated temp dir. Returns the repo path and a cleanup
// function that restores the original working directory and NM_HOME.
func setupTestRepo(t *testing.T) string {
	t.Helper()

	// Keep NM_HOME under a short temp root so the daemon socket path fits.
	repoDir := t.TempDir()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NM_TEST_START_DAEMON", "1")

	// Create a bare "origin" to use as the upstream.
	originDir := filepath.Join(t.TempDir(), "origin.git")
	run(t, "", "git", "init", "--bare", originDir)

	// Init repo and add origin.
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "remote", "add", "origin", originDir)

	// Create an initial commit so HEAD exists.
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")

	// Save and change to the repo dir.
	t.Chdir(repoDir)
	t.Cleanup(func() {
		p := paths.WithRoot(nmHome)
		_, _ = daemon.IsRunning(p)
		_ = daemon.Stop(p)
		// On Windows, the daemon may hold file locks briefly after stopping.
		if runtime.GOOS == "windows" {
			time.Sleep(500 * time.Millisecond)
		}
	})

	return repoDir
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", name, args, err, out)
	}
}

// chdir changes to the given directory and restores the original on cleanup.
func chdir(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
}

func executeCmd(args ...string) (string, error) {
	cmd := newRootCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func executeCmdWithContext(ctx context.Context, args ...string) (string, error) {
	cmd := newRootCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err := cmd.Execute()
	return buf.String(), err
}

func isMissingWorktreeError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "is not a working tree")
}

func TestIsMissingWorktreeError(t *testing.T) {
	t.Parallel()

	if isMissingWorktreeError(nil) {
		t.Fatal("nil should not be treated as a missing worktree error")
	}

	err := errors.New("git worktree remove --force C:\\temp\\worktree: exit status 128: fatal: 'C:\\temp\\worktree' is not a working tree")
	if !isMissingWorktreeError(err) {
		t.Fatal("expected missing worktree error to be ignored during cleanup")
	}

	err = errors.New("git worktree remove failed: permission denied")
	if isMissingWorktreeError(err) {
		t.Fatal("unexpected error should not be treated as a missing worktree error")
	}
}

func makeSocketSafeTempDir(t *testing.T) string {
	t.Helper()

	base := os.TempDir()
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "nmh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeTempRoot(t, dir) })
	return dir
}

// startTestDaemon starts an in-process daemon for integration tests.
func startTestDaemon(t *testing.T, p *paths.Paths, d *db.DB) {
	t.Helper()

	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)

	go func() {
		errCh <- daemon.RunWithResources(p, d)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := daemon.IsRunning(p); alive {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("daemon exited before becoming responsive: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if alive, _ := daemon.IsRunning(p); !alive {
		t.Fatal("daemon did not become responsive")
	}

	t.Cleanup(func() {
		_ = daemon.Stop(p)
		select {
		case <-errCh:
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop within 3s")
		}
	})
}

// removeTempRoot deletes a test's temp root and fails the test when it cannot.
// These roots are created with os.MkdirTemp rather than t.TempDir because a
// unix socket path has a small OS limit (~104 bytes on macOS) and t.TempDir
// embeds the full test name, so nothing else cleans them up. t.TempDir fails
// the test on a removal it cannot make, and so does this.
func removeTempRoot(t *testing.T, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("remove temp root %s: %v", dir, err)
	}
}
