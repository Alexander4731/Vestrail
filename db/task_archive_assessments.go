package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

var archiveAssessmentTables = []string{
	"goal_assessments", "coverage_plans", "coverage_checks", "coverage_attempts",
	"finding_verification_attempts", "finding_retests",
}

// v5 promises these audit records. A missing section must stop compaction and
// restore, rather than silently losing records or borrowing another task's IDs.
func validateArchiveAssessments(s *TaskArchiveSnapshot) error {
	if s.FormatVersion < 5 {
		return nil
	}
	tables := make(map[string]map[int64]map[string]any, len(archiveAssessmentTables))
	for _, table := range archiveAssessmentTables {
		raw := bytes.TrimSpace(s.Tables[table])
		if len(raw) == 0 || raw[0] != '[' || !json.Valid(raw) {
			return fmt.Errorf("%w: missing or invalid %s", ErrTaskArchiveFormatMismatch, table)
		}
		rows, err := decodeArchiveRows(raw)
		if err != nil {
			return err
		}
		if count, ok := s.DataCounts[table]; !ok || count != int64(len(rows)) {
			return fmt.Errorf("archive %s count mismatch", table)
		}
		byID := make(map[int64]map[string]any, len(rows))
		for _, row := range rows {
			id, ok := jsonInt64(row["id"])
			if !ok || id <= 0 || byID[id] != nil {
				return fmt.Errorf("archive %s has invalid or duplicate id", table)
			}
			byID[id] = row
		}
		tables[table] = byID
	}

	// Decode only identity columns from the potentially large graph/finding rows.
	var nodes []struct {
		ID            int64  `json:"id"`
		ExplorationID int64  `json:"exploration_id"`
		Kind          string `json:"kind"`
	}
	if err := json.Unmarshal(s.Tables["exploration_nodes"], &nodes); err != nil {
		return err
	}
	goals := map[int64]bool{}
	for _, n := range nodes {
		if n.ExplorationID == s.ExplorationID && n.Kind == KindGoal {
			goals[n.ID] = true
		}
	}
	var findings []struct {
		ID        int64  `json:"id"`
		TaskID    int64  `json:"task_id"`
		AttemptID *int64 `json:"verification_attempt_id"`
	}
	if err := json.Unmarshal(s.Tables["findings"], &findings); err != nil {
		return err
	}
	findingIDs := map[int64]bool{}
	for _, f := range findings {
		if f.TaskID == s.TaskID {
			findingIDs[f.ID] = true
		}
		if f.AttemptID != nil {
			a := tables["finding_verification_attempts"][*f.AttemptID]
			if id, ok := jsonInt64(a["finding_id"]); !ok || id != f.ID {
				return fmt.Errorf("archive finding verification projection mismatch")
			}
		}
	}
	for table, rows := range tables {
		for _, row := range rows {
			valid := false
			switch table {
			case "goal_assessments":
				expID, _ := jsonInt64(row["exploration_id"])
				goalID, _ := jsonInt64(row["goal_id"])
				valid = expID == s.ExplorationID && goals[goalID]
			case "coverage_plans":
				taskID, _ := jsonInt64(row["task_id"])
				valid = taskID == s.TaskID
			case "coverage_checks":
				planID, _ := jsonInt64(row["plan_id"])
				valid = tables["coverage_plans"][planID] != nil
			case "coverage_attempts":
				checkID, _ := jsonInt64(row["check_id"])
				valid = tables["coverage_checks"][checkID] != nil
			case "finding_verification_attempts":
				fid, _ := jsonInt64(row["finding_id"])
				valid = findingIDs[fid] && row["task_id"] == strconv.FormatInt(s.TaskID, 10) &&
					(row["state"] == "completed" || row["state"] == "interrupted") && row["finished_at"] != nil
				if row["verifier"] == "execution_receipt" && row["assurance"] != "none" {
					var refs []VerificationReceipt
					raw, _ := json.Marshal(row["evidence_refs"])
					valid = valid && json.Unmarshal(raw, &refs) == nil && len(refs) == 1 && refs[0].ID > 0 && refs[0].Version > 0
				}
			case "finding_retests":
				fid, _ := jsonInt64(row["finding_id"])
				valid = findingIDs[fid] && (row["status"] == "completed" || row["status"] == "failed" || row["status"] == "stopped") && row["finished_at"] != nil
			}
			if !valid {
				return fmt.Errorf("archive %s ownership or terminal state mismatch", table)
			}
		}
	}
	return nil
}
