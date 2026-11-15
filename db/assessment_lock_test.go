package db

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAssessmentWritesCancelWhileWaitingForTaskLock(t *testing.T) {
	d, task, store, goal, evidence := assessmentFixture(t)
	fid, err := d.AddFinding(task.ID, 0, "fixture", "lock fixture", "low", "claim", "proof", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.DeleteFinding(fid) })
	owner, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Rollback()
	if _, err = owner.Exec(`SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, task.ID); err != nil {
		t.Fatal(err)
	}
	for name, write := range map[string]func(context.Context) error{
		"coverage": func(ctx context.Context) error {
			_, err := d.CreateCoveragePlan(ctx, task.ID, "human:test", CoveragePlanInput{RequestKey: "blocked", Checks: []CoverageCheckSpec{{TargetRef: "fixture", Category: "local", TestID: "read", PreconditionVersion: 1}}})
			return err
		},
		"goal": func(ctx context.Context) error {
			return store.AssessGoal(ctx, GoalAssessmentInput{GoalID: goal, EvidenceID: evidence, Method: GoalModelAssessed, Actor: "agent:test"})
		},
		"verification": func(ctx context.Context) error {
			_, _, err := d.BeginVerification(ctx, fid, 0, "blocked", "evidence_integrity", "human:test", json.RawMessage(`{}`))
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		done := make(chan error, 1)
		go func() { done <- write(ctx) }()
		select {
		case err := <-done:
			if err == nil || ctx.Err() == nil {
				t.Errorf("%s did not report cancellation: %v", name, err)
			}
		case <-time.After(3 * time.Second):
			cancel()
			owner.Rollback()
			<-done
			t.Fatalf("%s ignored cancellation while waiting for a row lock", name)
		}
		cancel()
	}
}
