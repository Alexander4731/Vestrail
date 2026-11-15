package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Alexander4731/Vestrail/db"
	"github.com/Alexander4731/Vestrail/verification"
)

func (s *Server) listFindingVerifications(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, ok := s.findingTrafficAccess(w, r, false)
	if !ok {
		return
	}
	var before int64
	if raw := r.URL.Query().Get("before"); raw != "" {
		var err error
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before <= 0 {
			writeErr(w, 400, "invalid cursor")
			return
		}
	}
	items, err := s.m.pg.ListVerifications(r.Context(), id, before)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"attempts": items, "verifiers": verification.Registry()})
}

func (s *Server) startFindingVerification(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	id, ok := s.findingTrafficAccess(w, r, true)
	if !ok {
		return
	}
	var req verification.Request
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, 400, "invalid verification request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "expected one JSON object")
		return
	}
	result, created, err := (verification.Service{Store: s.evidenceStore()}).Run(r.Context(), id, "human:Vestrail", req)
	if err != nil {
		status := 500
		switch {
		case errors.Is(err, db.ErrVerificationInput):
			status = 400
		case errors.Is(err, db.ErrFindingNotFound):
			status = 404
		case errors.Is(err, db.ErrVerificationConflict), errors.Is(err, db.ErrEvidenceConflict), errors.Is(err, db.ErrTaskArchiveState):
			status = 409
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"attempt": result, "created": created})
}
