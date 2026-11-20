package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Alexander4731/Vestrail/db"
)

func (s *Server) taskUsageAudit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid task id")
		return
	}
	hours := 0
	if raw := r.URL.Query().Get("hours"); raw != "" {
		hours, err = strconv.Atoi(raw)
		if err != nil || hours < 0 || hours > 8760 {
			writeErr(w, http.StatusBadRequest, "hours must be between 0 and 8760")
			return
		}
	}
	pg := s.m.PG()
	if pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "用量分析需要计量数据库")
		return
	}
	var since *time.Time
	if hours > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
		since = &cutoff
	}
	audit, err := pg.TaskUsageAudit(r.Context(), id, since)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "task not found")
		return
	}
	if errors.Is(err, db.ErrUsageAuditArchived) {
		writeErr(w, http.StatusConflict, "任务已归档，在线计量明细不可用；查看此报告不会恢复任务")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, audit)
}
