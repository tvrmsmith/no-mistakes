package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// legacyDoctorHealthResult mimics a pre-handshake daemon whose health
// response carries no protocol_version field at all.
type legacyDoctorHealthResult struct {
	Status string `json:"status"`
}

func TestDoctorReportsVersionMismatchInsteadOfStopped(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	// Unix domain socket paths have a small OS limit, so use a short-named
	// temp dir rather than t.TempDir() (which embeds the full test name).
	nmHome, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(nmHome)
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	t.Setenv("PATH", binDir)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodHealth, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return legacyDoctorHealthResult{Status: "ok"}, nil
	})
	if err := srv.Listen(p.Socket()); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.ServeReady() }()
	defer srv.Close()

	out, _ := executeCmd("doctor")

	mismatch := &ipc.VersionMismatchError{Local: ipc.ProtocolVersion, Remote: 0, RemoteRole: ipc.RoleDaemon}
	lines := strings.Split(out, "\n")
	row := -1
	for i, line := range lines {
		if strings.Contains(line, "daemon        ") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("doctor printed no daemon row, got:\n%s", out)
	}

	// The checklist row carries one- or two-word values everywhere else, so the
	// mismatch belongs there as a short state with its remedy on its own line.
	if !strings.Contains(lines[row], mismatch.Summary()) {
		t.Errorf("daemon row should state %q, got: %q", mismatch.Summary(), lines[row])
	}
	if strings.Contains(lines[row], "stopped") {
		t.Errorf("doctor should not report a skewed daemon as stopped, got: %q", lines[row])
	}
	if strings.Contains(lines[row], mismatch.Remedy()) {
		t.Errorf("the remedy should not be crammed into the checklist row, got: %q", lines[row])
	}

	if row+1 >= len(lines) {
		t.Fatalf("doctor printed no remedy line after the daemon row, got:\n%s", out)
	}
	remedy := lines[row+1]
	if !strings.Contains(remedy, mismatch.Remedy()) {
		t.Errorf("remedy line should name the fix %q, got: %q", mismatch.Remedy(), remedy)
	}
	if strings.TrimLeft(remedy, " ") == remedy {
		t.Errorf("remedy line should be indented under the daemon row, got: %q", remedy)
	}
}
