package branchsync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// probeRepo is a repository with one commit and one private ref, which is the
// smallest shape that can tell the three probe answers apart.
func probeRepo(t *testing.T) (dir, sha, ref string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "probe")
	mustRun(t, filepath.Dir(dir), "init", "-b", "main", dir)
	configureIdentity(t, dir)
	mustWrite(t, filepath.Join(dir, "file.txt"), "probe\n")
	mustRun(t, dir, "add", "file.txt")
	mustRun(t, dir, "commit", "-m", "probe")
	sha = mustRun(t, dir, "rev-parse", "HEAD")
	ref = "refs/no-mistakes/probe/present"
	mustRun(t, dir, "update-ref", ref, sha)
	return dir, sha, ref
}

// TestReadRefDetachedSeparatesAbsenceFromAnUnanswerableProbe pins the
// difference the record depends on: git reporting a ref does not exist is
// knowledge, while a probe that could not run at all is not. Collapsing the two
// - reading any probe failure as an empty ref - is what makes a refusal
// understate what the operator can still find.
func TestReadRefDetachedSeparatesAbsenceFromAnUnanswerableProbe(t *testing.T) {
	t.Parallel()

	dir, sha, ref := probeRepo(t)
	ctx := context.Background()

	if got, known := readRefDetached(ctx, dir, ref); got != sha || !known {
		t.Fatalf("existing ref probe = (%q, %v), want (%q, true)", got, known, sha)
	}

	if got, known := readRefDetached(ctx, dir, "refs/no-mistakes/probe/absent"); got != "" || !known {
		t.Fatalf("absent ref probe = (%q, %v), want (\"\", true): git's own exit-1 report is knowledge", got, known)
	}

	outside := t.TempDir()
	if got, known := readRefDetached(ctx, outside, ref); got != "" || known {
		t.Fatalf("unrunnable probe = (%q, %v), want (\"\", false): a probe that could not run proves nothing", got, known)
	}
}

// TestWriteRefAndRecordReportsWhatAnUnanswerableProbeCannotDeny covers both
// branches that depend on that distinction. When the probe cannot answer, the
// ref is recorded whether the write reported success or failure, because a
// refusal may overstate what the operator can find and must never understate
// it. Only a probe that proved the ref absent may keep a failed write out of
// the record.
func TestWriteRefAndRecordReportsWhatAnUnanswerableProbeCannotDeny(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	outside := t.TempDir()
	const ref = "refs/no-mistakes/probe/unanswerable"
	writeFailed := errors.New("write failed")

	var recorded []string
	record := func(r string) { recorded = append(recorded, r) }

	if err := writeRefAndRecord(ctx, outside, ref, record, func() error { return nil }); err != nil {
		t.Fatalf("write returned %v", err)
	}
	if len(recorded) != 1 || recorded[0] != ref {
		t.Fatalf("recorded = %v, want the ref after a successful write no probe could verify", recorded)
	}

	recorded = nil
	if err := writeRefAndRecord(ctx, outside, ref, record, func() error { return writeFailed }); !errors.Is(err, writeFailed) {
		t.Fatalf("write error = %v, want it returned unchanged", err)
	}
	if len(recorded) != 1 || recorded[0] != ref {
		t.Fatalf("recorded = %v, want the ref after a failed write no probe could deny", recorded)
	}

	dir, _, _ := probeRepo(t)
	recorded = nil
	if err := writeRefAndRecord(ctx, dir, "refs/no-mistakes/probe/never-written", record, func() error { return writeFailed }); !errors.Is(err, writeFailed) {
		t.Fatalf("write error = %v, want it returned unchanged", err)
	}
	if len(recorded) != 0 {
		t.Fatalf("recorded = %v, want nothing: the probe proved the ref absent", recorded)
	}
}
