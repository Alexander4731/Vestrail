package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Alexander4731/Vestrail/db"
)

func coverageError(w http.ResponseWriter, err error) {
	status := 500
	switch {
	case errors.Is(err, sql.ErrNoRows):
		status = 404
	case errors.Is(err, db.ErrCoverageInput):
		status = 400
	case errors.Is(err, db.ErrCoverageConflict), errors.Is(err, db.ErrTaskArchiveState):
		status = 409
	}
	writeErr(w, status, err.Error())
}

func decodeCoverage(w http.ResponseWriter, r *http.Request, out any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		writeErr(w, 400, "invalid coverage request")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "expected one JSON object")
		return false
	}
	return true
}

func (s *Server) coverageTaskID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if s.pg(w) == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid task id")
		return 0, false
	}
	task, err := s.m.pg.GetTask(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return 0, false
	}
	if task == nil {
		writeErr(w, 404, "task not found")
		return 0, false
	}
	return id, true
}

func (s *Server) getCoverageLedger(w http.ResponseWriter, r *http.Request) {
	id, ok := s.coverageTaskID(w, r)
	if !ok {
		return
	}
	values := map[string]int64{"plan_id": 0, "after": 0}
	for key := range values {
		if raw := r.URL.Query().Get(key); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v <= 0 {
				writeErr(w, 400, "invalid coverage cursor")
				return
			}
			values[key] = v
		}
	}
	out, err := s.m.pg.CoverageLedger(r.Context(), id, values["plan_id"], values["after"])
	if err != nil {
		coverageError(w, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) createCoveragePlan(w http.ResponseWriter, r *http.Request) {
	id, ok := s.coverageTaskID(w, r)
	if !ok {
		return
	}
	var in db.CoveragePlanInput
	if !decodeCoverage(w, r, &in) {
		return
	}
	p, err := s.m.pg.CreateCoveragePlan(r.Context(), id, "human:Vestrail", in)
	if err != nil {
		coverageError(w, err)
		return
	}
	writeJSON(w, 201, p)
}

func (s *Server) recordCoverageAttempt(w http.ResponseWriter, r *http.Request) {
	id, ok := s.coverageTaskID(w, r)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(r.PathValue("check_id"), 10, 64)
	if err != nil || cid <= 0 {
		writeErr(w, 400, "invalid check id")
		return
	}
	var in db.CoverageAttemptInput
	if !decodeCoverage(w, r, &in) {
		return
	}
	if err = s.m.pg.RecordCoverageReport(r.Context(), id, cid, "human:Vestrail", true, in); err != nil {
		coverageError(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"recorded": true})
}
