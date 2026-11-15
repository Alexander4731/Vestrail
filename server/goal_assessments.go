package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Alexander4731/Vestrail/db"
)

func (s *Server) acceptGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "任务正在删除")
		return
	}
	defer s.engine.decInflight(t.ID)
	goalID, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || goalID <= 0 {
		writeErr(w, 400, "invalid goal id")
		return
	}
	var input struct {
		EvidenceID string `json:"evidence_id"`
		Reason     string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := decode(r, &input); err != nil {
		writeErr(w, 400, "invalid assessment")
		return
	}
	evidenceID, err := strconv.ParseInt(input.EvidenceID, 10, 64)
	if err != nil || evidenceID <= 0 || strings.TrimSpace(input.Reason) == "" {
		writeErr(w, 400, "需要证据 ID 与人工验收理由")
		return
	}
	if isTerminalStatus(t.lifecycleSnapshot().Status) {
		writeErr(w, 409, "请先重新开启任务再进行新的目标验收")
		return
	}
	err = t.Store.AssessGoal(r.Context(), db.GoalAssessmentInput{GoalID: goalID, EvidenceID: evidenceID, Method: db.GoalHumanAccepted, Actor: "human:Vestrail", Reason: input.Reason})
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "goal not found")
		return
	}
	if err != nil {
		status := 500
		if errors.Is(err, db.ErrGoalAssessmentInput) {
			status = 400
		}
		if errors.Is(err, db.ErrTaskArchiveState) {
			status = 409
		}
		writeErr(w, status, err.Error())
		return
	}
	node, err := t.Store.GetNode(goalID)
	if err != nil || node == nil {
		writeErr(w, 500, "目标读取失败")
		return
	}
	if err = t.Store.AttachGoalAssessments(r.Context(), []*db.Node{node}); err != nil {
		writeErr(w, 500, "判定读取失败")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

func (s *Server) goalAssessmentSnapshots(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("aid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid assessment id")
		return
	}
	goal, evidence, err := t.Store.GoalSnapshots(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "assessment not found")
		return
	}
	if err != nil {
		writeErr(w, 500, "判定快照读取失败")
		return
	}
	writeJSON(w, 200, map[string]any{"goal": goal, "evidence": evidence})
}
