package cli

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// setupDoctorTrustEnvWithoutDatabase points NM_HOME and HOME at a fresh
// directory and leaves the database file absent, which is the fresh-install
// state.
func setupDoctorTrustEnvWithoutDatabase(t *testing.T) *paths.Paths {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("NM_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", "")

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New() error = %v", err)
	}
	return p
}

// setupDoctorTrustEnv additionally creates the database and registers
// repoCount repositories, so repoCount == 0 means an existing database with an
// empty repos table rather than no database at all.
func setupDoctorTrustEnv(t *testing.T, repoCount int) (gatePaths []string) {
	t.Helper()
	p := setupDoctorTrustEnvWithoutDatabase(t)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer d.Close()
	ids := []string{"a", "b", "c"}
	for i := 0; i < repoCount; i++ {
		if _, err := d.InsertRepoWithID(ids[i], "/work/"+ids[i], "https://example.com/"+ids[i]+".git", "main"); err != nil {
			t.Fatalf("InsertRepoWithID(%q) error = %v", ids[i], err)
		}
		gatePaths = append(gatePaths, p.RepoDir(ids[i]))
	}
	return gatePaths
}

func TestDoctorClaudeTrust_NoConfigReportsAllGateRepositoriesUntrusted(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	gatePaths := setupDoctorTrustEnv(t, 2)
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude trust") {
		t.Errorf("doctor output should contain %q, got:\n%s", "claude trust", out)
	}
	for _, gp := range gatePaths {
		if !strings.Contains(out, gp) {
			t.Errorf("doctor output should name gate path %q, got:\n%s", gp, out)
		}
	}
	if !strings.Contains(out, "run claude interactively") {
		t.Errorf("doctor output should contain remedy, got:\n%s", out)
	}
	if strings.Contains(out, "some checks failed") {
		t.Errorf("the trust report is a warning and must never fail doctor, got:\n%s", out)
	}
}

// TestDoctorClaudeTrust_ReportIsIndependentOfTheConfiguredAgent pins that the
// row appears whatever `agent` says globally. The global is routinely `auto`,
// each repository can override it from its own trusted default branch, and
// doctor reads neither of those per-repo values, so keying the report on the
// global would hide the condition from exactly the operator who hits it.
func TestDoctorClaudeTrust_ReportIsIndependentOfTheConfiguredAgent(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	setupDoctorTrustEnv(t, 2)
	nmHome := os.Getenv("NM_HOME")
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("agent: codex\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	writeDoctorStubBinary(t, binDir, "codex")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude trust") {
		t.Errorf("doctor output should contain %q, got:\n%s", "claude trust", out)
	}
	if !strings.Contains(out, "untrusted") {
		t.Errorf("doctor should still report the untrusted gate repositories, got:\n%s", out)
	}
	if strings.Contains(out, "some checks failed") {
		t.Errorf("doctor should not fail overall for a non-claude agent, got:\n%s", out)
	}
}

func TestDoctorClaudeTrust_AbsentFromProjectsIsReportedUntrusted(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	gatePaths := setupDoctorTrustEnv(t, 2)
	nmHome := os.Getenv("NM_HOME")
	home := os.Getenv("HOME")
	configJSON := `{"projects":{"` + gatePaths[0] + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write claude config: %v", err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)
	_ = nmHome

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if strings.Contains(out, gatePaths[0]) {
		t.Errorf("doctor should not name the already-trusted gate path %q, got:\n%s", gatePaths[0], out)
	}
	if !strings.Contains(out, gatePaths[1]) {
		t.Errorf("doctor should name the untrusted gate path %q, got:\n%s", gatePaths[1], out)
	}
	if !strings.Contains(out, "1 gate repository untrusted") {
		t.Errorf("doctor output should report exactly one untrusted repository, got:\n%s", out)
	}
}

func TestDoctorClaudeTrust_AllGateRepositoriesTrusted(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	gatePaths := setupDoctorTrustEnv(t, 3)
	home := os.Getenv("HOME")
	entries := make([]string, len(gatePaths))
	for i, gp := range gatePaths {
		entries[i] = `"` + gp + `":{"hasTrustDialogAccepted":true}`
	}
	configJSON := `{"projects":{` + strings.Join(entries, ",") + `}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write claude config: %v", err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "3 gate repositories trusted") {
		t.Errorf("doctor output should contain %q, got:\n%s", "3 gate repositories trusted", out)
	}
	if strings.Contains(out, "untrusted") {
		t.Errorf("doctor should not mention untrusted, got:\n%s", out)
	}
	if strings.Contains(out, "some checks failed") {
		t.Errorf("doctor should not fail overall when trusted, got:\n%s", out)
	}
}

func TestDoctorClaudeTrust_ClaudeNotOnPathOmitsCheck(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	setupDoctorTrustEnv(t, 2)
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "claude trust") {
		t.Errorf("doctor output should not contain %q when claude is absent, got:\n%s", "claude trust", out)
	}
}

func TestDoctorClaudeTrust_ZeroRepositoriesOmitsCheck(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	setupDoctorTrustEnv(t, 0)
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "claude trust") {
		t.Errorf("doctor output should not contain %q when no repos are registered, got:\n%s", "claude trust", out)
	}
}

// TestDoctorClaudeTrust_DoesNotCreateTheDatabase pins that the trust check is
// read-only. Opening the database for write creates the file and runs every
// migration, so the "database" row would report "not found (will be created on
// first use)" on the first doctor run and a created database on the second,
// purely because doctor itself ran.
func TestDoctorClaudeTrust_DoesNotCreateTheDatabase(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	p := setupDoctorTrustEnvWithoutDatabase(t)
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(p.DB()); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) error = %v, want a not-exist error: doctor must not create the database", p.DB(), err)
	}
	if strings.Contains(out, "claude trust") {
		t.Errorf("doctor output should not contain %q with no database, got:\n%s", "claude trust", out)
	}
}

// TestDoctorClaudeTrust_UnmigratedDatabaseOmitsCheck covers the cost of
// reading read-only: a database written by an older build is missing a column
// GetRepos names, which is the ordinary state between an upgrade and the next
// writer's migrations. It must not surface as an alarming "cannot list gate
// repositories" warning. The check is driven directly rather than through the
// doctor command, because doctor's own "database" row opens the file for write
// and would migrate it before this check ever reads it.
func TestDoctorClaudeTrust_UnmigratedDatabaseOmitsCheck(t *testing.T) {
	setupDoctorTrustEnv(t, 2)
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New() error = %v", err)
	}
	sqlDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := sqlDB.Exec(`ALTER TABLE repos DROP COLUMN fork_url`); err != nil {
		sqlDB.Close()
		t.Fatalf("drop fork_url: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	binDir := t.TempDir()
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	var reported []string
	record := func(label, detail string) { reported = append(reported, label+" "+detail) }
	doctorClaudeWorkspaceTrust(p, record, record)

	if len(reported) != 0 {
		t.Errorf("doctorClaudeWorkspaceTrust reported %v, want nothing for a database one migration behind", reported)
	}
}

// A genuine read failure is still reported: the migration-lag exemption must
// not swallow every GetRepos error.
func TestDoctorClaudeTrust_UnreadableRepoTableIsReported(t *testing.T) {
	setupDoctorTrustEnv(t, 2)
	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New() error = %v", err)
	}
	sqlDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := sqlDB.Exec(`UPDATE repos SET created_at = 'not-a-timestamp'`); err != nil {
		sqlDB.Close()
		t.Fatalf("corrupt created_at: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	binDir := t.TempDir()
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	var reported []string
	record := func(label, detail string) { reported = append(reported, detail) }
	doctorClaudeWorkspaceTrust(p, record, record)

	if len(reported) == 0 {
		t.Fatal("doctorClaudeWorkspaceTrust reported nothing, want the read failure surfaced")
	}
}

func TestDoctorClaudeTrust_MalformedConfigIsUnreadable(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	setupDoctorTrustEnv(t, 2)
	home := os.Getenv("HOME")
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("write claude config: %v", err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "claude")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unreadable Claude Code config at") {
		t.Errorf("doctor output should contain %q, got:\n%s", "unreadable Claude Code config at", out)
	}
}
