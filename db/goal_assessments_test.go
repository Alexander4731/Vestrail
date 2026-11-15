package db

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
)

func assessmentFixture(t *testing.T) (*DB, *Task, *ExplorationStore, int64, int64) {
	d, task := reportTestTask(t)
	s := d.Exploration(task.ExplorationID)
	goal, err := s.AddGoal(map[string]any{"text": "confirm the owned resource"}, "human")
	if err != nil {
		t.Fatal(err)
	}
	ev, err := s.AddNode(KindFact, map[string]any{"summary": "owner baseline observed"}, 0, "confirmed", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	return d, task, s, goal, ev
}

func latestAssessment(t *testing.T, s *ExplorationStore, goal int64) *GoalAssessment {
	t.Helper()
	n, err := s.GetNode(goal)
	if err != nil || n == nil {
		t.Fatalf("node %v %v", n, err)
	}
	if err = s.AttachGoalAssessments(t.Context(), []*Node{n}); err != nil {
		t.Fatal(err)
	}
	if n.GoalAssessment == nil {
		t.Fatal("assessment missing")
	}
	return n.GoalAssessment
}

func TestGoalAssessmentReasonIdempotencyAndFreshness(t *testing.T) {
	d, _, s, goal, ev := assessmentFixture(t)
	in := GoalAssessmentInput{GoalID: goal, EvidenceID: ev, Method: GoalModelAssessed, Actor: "agent:test", Reason: "owner and baseline agree"}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := s.AssessGoal(t.Context(), in); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	a := latestAssessment(t, s, goal)
	if a.Reason != in.Reason || a.Method != GoalModelAssessed || a.Stale || a.MissingReason {
		t.Fatalf("wrong assessment %+v", a)
	}
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM goal_assessments WHERE goal_id=$1`, goal).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate delivery: %d %v", count, err)
	}
	_, before, err := s.GoalSnapshots(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Even an old writer that forgets to increment the evidence revision must
	// not keep an obsolete payload marked current.
	if _, err = d.Exec(`UPDATE exploration_nodes SET payload='{"summary":"changed"}' WHERE id=$1`, ev); err != nil {
		t.Fatal(err)
	}
	if !latestAssessment(t, s, goal).Stale {
		t.Fatal("changed evidence stayed current")
	}
	_, after, err := s.GoalSnapshots(t.Context(), a.ID)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("audit snapshot mutated")
	}
	if err = s.UpdateGoalPayload(goal, "different goal", ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.GetNode(goal)
	if err != nil || n.State != "open" {
		t.Fatalf("edited goal: %+v %v", n, err)
	}
}

func TestGoalAssessmentAuthorityAndOwnership(t *testing.T) {
	d, _, s, goal, ev := assessmentFixture(t)
	for _, in := range []GoalAssessmentInput{
		{GoalID: goal, EvidenceID: ev, Method: "machine_checked", Actor: "agent:spoof", Reason: "verified"},
		{GoalID: goal, EvidenceID: ev, Method: GoalHumanAccepted, Actor: "human:test"},
		{GoalID: goal, EvidenceID: goal, Method: GoalModelAssessed, Actor: "agent:test"},
	} {
		if err := s.AssessGoal(t.Context(), in); err == nil {
			t.Fatalf("invalid input accepted %+v", in)
		}
	}
	other, err := d.CreateTask("other", "other", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(other.ID)
	x, err := d.Exploration(other.ExplorationID).AddNode(KindFact, map[string]any{"summary": "not visible"}, 0, "confirmed", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AssessGoal(t.Context(), GoalAssessmentInput{GoalID: goal, EvidenceID: x, Method: GoalModelAssessed, Actor: "agent:test"}); err == nil {
		t.Fatal("cross-task evidence accepted")
	}
	if err = s.ProveGoal(goal, ev); err != nil {
		t.Fatal(err)
	}
	a := latestAssessment(t, s, goal)
	if !a.MissingReason || a.Method != GoalModelAssessed {
		t.Fatalf("legacy method not explicit %+v", a)
	}
	if _, _, err = d.Exploration(other.ExplorationID).GoalSnapshots(t.Context(), a.ID); err == nil {
		t.Fatal("snapshot leaked across tasks")
	}
}

func TestGoalAssessmentArchiveRoundTrip(t *testing.T) {
	d, task, s, goal, ev := assessmentFixture(t)
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	runtimeLock, err := d.AcquireRuntimeLock(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeLock.Close()
	if err := s.AssessGoal(t.Context(), GoalAssessmentInput{GoalID: goal, EvidenceID: ev, Method: GoalHumanAccepted, Actor: "human:test", Reason: "reviewed original evidence"}); err != nil {
		t.Fatal(err)
	}
	want := latestAssessment(t, s, goal)
	coverage := coveragePlanFixture(t, d, task, 1)
	checks, err := d.CoverageLedger(t.Context(), task.ID, coverage.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.RecordCoverageReport(t.Context(), task.ID, checks.Items[0].ID, "human:test", true, CoverageAttemptInput{RequestKey: "archive-block", Outcome: "blocked", Reason: "missing session"}); err != nil {
		t.Fatal(err)
	}
	fid, err := d.AddFinding(task.ID, 0, "fixture", "archive finding", "low", "claim", "evidence", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteFinding(fid) })
	attempt, _, err := d.BeginVerification(t.Context(), fid, 0, "archive", "evidence_integrity", "human:test", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.FinishVerification(t.Context(), attempt.ID, VerificationDecision{Outcome: "supported", Assurance: "evidence_bound", Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	retest := completedRetest(t, d, fid)
	if err = d.AcceptFindingRetest(t.Context(), fid, retest.ID, "human:test", "reviewed archive fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET paused=true WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	a, err := d.QueueTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AssessGoal(t.Context(), GoalAssessmentInput{GoalID: goal, EvidenceID: ev, Method: GoalHumanAccepted, Actor: "human:test", Reason: "late archive write"}); !errors.Is(err, ErrTaskArchiveState) {
		t.Fatal("queued archive accepted a new assessment", err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.SnapshotTaskArchive(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FormatVersion != 5 || snapshot.DataCounts["goal_assessments"] != 1 {
		t.Fatalf("missing archive audit: %+v", snapshot.DataCounts)
	}
	if snapshot.DataCounts["finding_verification_attempts"] != 1 || snapshot.DataCounts["finding_retests"] != 1 {
		t.Fatal("missing verification/retest archive")
	}
	var data []json.RawMessage
	if err = json.Unmarshal(snapshot.Tables["goal_assessments"], &data); err != nil || len(data) != 1 {
		t.Fatal("audit missing")
	}
	for _, table := range archiveAssessmentTables {
		incomplete := *snapshot
		incomplete.Tables = maps.Clone(snapshot.Tables)
		delete(incomplete.Tables, table)
		if err = d.CompleteTaskArchive(a.ID, &incomplete, "fixture.tar.zst", "test", 1, 1); !errors.Is(err, ErrTaskArchiveFormatMismatch) {
			t.Fatalf("incomplete %s archive permitted compaction: %v", table, err)
		}
	}
	if err = d.CompleteTaskArchive(a.ID, snapshot, "fixture.tar.zst", "test", 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = d.QueueTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = d.ClaimTaskArchiveJob(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range archiveAssessmentTables {
		corrupt := *snapshot
		corrupt.Tables = maps.Clone(snapshot.Tables)
		rows, err := decodeArchiveRows(corrupt.Tables[table])
		if err != nil || len(rows) != 1 {
			t.Fatalf("missing %s fixture: %v", table, err)
		}
		owner := map[string]string{"goal_assessments": "goal_id", "coverage_plans": "task_id", "coverage_checks": "plan_id", "coverage_attempts": "check_id", "finding_verification_attempts": "finding_id", "finding_retests": "finding_id"}[table]
		rows[0][owner] = -1
		corrupt.Tables[table], err = json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = d.RestoreTaskArchive(a.ID, &corrupt, 0); err == nil {
			t.Fatalf("corrupt %s ownership permitted restore", table)
		}
	}
	if _, err = d.RestoreTaskArchive(a.ID, snapshot, 0); err != nil {
		t.Fatal(err)
	}
	if err = d.CompleteTaskArchiveRestore(a.ID); err != nil {
		t.Fatal(err)
	}
	got := latestAssessment(t, s, goal)
	if got.ID != want.ID || got.Reason != want.Reason || got.EvidenceSHA256 != want.EvidenceSHA256 || got.Stale {
		t.Fatalf("restored audit differs: want=%+v got=%+v", want, got)
	}
	restored, err := d.GetVerification(t.Context(), fid, attempt.ID)
	if err != nil || restored.Assurance != "evidence_bound" || restored.Reason != "fixture" {
		t.Fatalf("restored verification %+v %v", restored, err)
	}
	checks, err = d.CoverageLedger(t.Context(), task.ID, coverage.ID, 0)
	if err != nil || checks.ScopeStale || checks.Total != 1 || checks.Items[0].Reason != "missing session" {
		t.Fatalf("coverage restore %+v %v", checks, err)
	}
	retests, err := d.ListFindingRetests(fid)
	if err != nil || len(retests) != 1 || retests[0].ApplicationReason != "reviewed archive fixture" || retests[0].ConversationID != nil {
		t.Fatalf("restored retest %+v %v", retests, err)
	}
}

func TestGoalAssessmentRejectsOversizedSnapshots(t *testing.T) {
	d, _, s, goal, ev := assessmentFixture(t)
	if _, err := d.Exec(`UPDATE exploration_nodes SET payload=jsonb_build_object('summary',$2::text) WHERE id=$1`, ev, strings.Repeat("x", ReportPayloadLimit)); err != nil {
		t.Fatal(err)
	}
	if err := s.AssessGoal(t.Context(), GoalAssessmentInput{GoalID: goal, EvidenceID: ev, Method: GoalModelAssessed, Actor: "agent:test"}); !errors.Is(err, ErrGoalAssessmentInput) {
		t.Fatal("oversized snapshot accepted", err)
	}
	n, err := s.GetNode(goal)
	if err != nil || n.State != "open" {
		t.Fatalf("rejected assessment changed goal: %+v %v", n, err)
	}
}
