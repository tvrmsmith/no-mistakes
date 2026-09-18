package gate

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMirrorPublicationReviewCertifiedSupersession covers ADR 0002. The fixture
// is the deadlock the ADR exists for: a run publishes a head, a CI merge-conflict
// repair rebases the branch so the new head neither descends from the published
// one nor keeps its patch identities, revalidation re-certifies the rebased head,
// and the mirror still holds the pre-repair head. Only the two ADR 0002
// conditions together release it.
func TestMirrorPublicationReviewCertifiedSupersession(t *testing.T) {
	for _, variant := range []string{
		"published_and_reviewed",
		"published_without_review_approval",
		"review_approval_of_another_head",
		"reviewed_but_not_published_by_this_run",
		"another_runs_published_head",
	} {
		t.Run(variant, func(t *testing.T) {
			work := initReconcileRepo(t)
			base := reconcileGit(t, work, "rev-parse", "HEAD")

			// The head this run pushed before CI found the conflict.
			writeReconcileFile(t, work, "feature.txt", "feature work\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "feature work")
			publishedHead := reconcileGit(t, work, "rev-parse", "HEAD")

			// The repaired head. Rebased onto a moved base with the conflict
			// resolved, so it descends from neither the published head nor
			// shares its patch identity.
			reconcileGit(t, work, "checkout", "--detach", base)
			writeReconcileFile(t, work, "feature.txt", "feature work, conflict resolved\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "feature work rebased")
			repairedHead := reconcileGit(t, work, "rev-parse", "HEAD")

			// A head that belongs to some other run.
			reconcileGit(t, work, "checkout", "--detach", base)
			writeReconcileFile(t, work, "other.txt", "another run\n")
			reconcileGit(t, work, "add", "-A")
			reconcileGit(t, work, "commit", "-m", "another run")
			foreignHead := reconcileGit(t, work, "rev-parse", "HEAD")

			evidence := MirrorPublicationEvidence{
				PublishedHead:      publishedHead,
				ReviewApprovedHead: repairedHead,
			}
			switch variant {
			case "published_without_review_approval":
				evidence.ReviewApprovedHead = ""
			case "review_approval_of_another_head":
				evidence.ReviewApprovedHead = publishedHead
			case "reviewed_but_not_published_by_this_run":
				evidence.PublishedHead = ""
			case "another_runs_published_head":
				evidence.PublishedHead = foreignHead
			}

			gateDir := filepath.Join(t.TempDir(), "gate.git")
			reconcileGit(t, "", "init", "--bare", gateDir)
			reconcileGit(t, gateDir, "fetch", work, publishedHead+":refs/heads/feature")

			plan, err := PlanMirrorPublicationReconciliation(t.Context(), gateDir, work, "feature", repairedHead, evidence)
			if variant != "published_and_reviewed" {
				if err == nil || plan.Reconcile {
					t.Fatalf("mirror head superseded without both conditions: plan=%+v err=%v", plan, err)
				}
				if !strings.Contains(err.Error(), publishedHead) {
					t.Fatalf("refusal lost the at-risk commit %s: %v", publishedHead, err)
				}
				return
			}
			if err != nil || !plan.Reconcile {
				t.Fatalf("review-certified supersession refused: plan=%+v err=%v", plan, err)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != publishedHead {
				t.Fatalf("planning moved mirror to %s, want %s", got, publishedHead)
			}

			// Nothing is destroyed: the archive tag holds the exact superseded
			// head before the branch ref makes way for the reviewed successor.
			result, err := ApplyStaleBranchReconciliation(t.Context(), gateDir, plan)
			if err != nil || !result.Reconciled {
				t.Fatalf("apply = %+v, err = %v", result, err)
			}
			if got := reconcileGit(t, gateDir, "rev-parse", result.ArchivedTag); got != publishedHead {
				t.Fatalf("archive = %s, want superseded published head %s", got, publishedHead)
			}
			reconcileGit(t, work, "push", gateDir, repairedHead+":refs/heads/feature")
			if got := reconcileGit(t, gateDir, "rev-parse", "refs/heads/feature"); got != repairedHead {
				t.Fatalf("ordinary push = %s, want %s", got, repairedHead)
			}
		})
	}
}

// TestPlanStaleBranchReconciliationDeniesReviewCertifiedEvidence pins that the
// ADR 0002 exception is publication-only. An AXI submission reaches the guard
// through PlanStaleBranchReconciliation, which can present the submitted head
// and nothing else.
func TestPlanStaleBranchReconciliationDeniesReviewCertifiedEvidence(t *testing.T) {
	work := initReconcileRepo(t)
	base := reconcileGit(t, work, "rev-parse", "HEAD")
	writeReconcileFile(t, work, "feature.txt", "feature work\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "feature work")
	publishedHead := reconcileGit(t, work, "rev-parse", "HEAD")
	reconcileGit(t, work, "checkout", "--detach", base)
	writeReconcileFile(t, work, "feature.txt", "feature work, conflict resolved\n")
	reconcileGit(t, work, "add", "-A")
	reconcileGit(t, work, "commit", "-m", "feature work rebased")
	repairedHead := reconcileGit(t, work, "rev-parse", "HEAD")

	gateDir := filepath.Join(t.TempDir(), "gate.git")
	reconcileGit(t, "", "init", "--bare", gateDir)
	reconcileGit(t, gateDir, "fetch", work, publishedHead+":refs/heads/feature")

	plan, err := PlanStaleBranchReconciliation(t.Context(), gateDir, work, "feature", repairedHead, publishedHead)
	if err != nil || !plan.Reconcile {
		t.Fatalf("submitted-head exception refused for an AXI caller: plan=%+v err=%v", plan, err)
	}
	if plan.PreserveDescendantOf != "" {
		t.Fatalf("AXI plan preserved descendants: %+v", plan)
	}
}
