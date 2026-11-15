package server

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestTaskReportRejectsInvalidTaskAndShowsReadFailure(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, err := m.CreateTask("report handler", "report goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m}
	for _, id := range []string{"-1", "garbage", "0"} {
		w := httptest.NewRecorder()
		s.getReport(w, httptest.NewRequest("GET", "/api/report?task="+id, nil))
		if w.Code != 400 {
			t.Fatalf("task %q: %d %s", id, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	s.getReport(w, httptest.NewRequest("GET", "/api/report?task="+task.ID, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "不代表测试完整") {
		t.Fatalf("report: %d %s", w.Code, w.Body.String())
	}
	taskID, _ := strconv.ParseInt(task.ID, 10, 64)
	if err = m.pg.DeleteTask(taskID); err != nil {
		t.Fatal(err)
	}
	if err = m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.getReport(w, httptest.NewRequest("GET", "/api/report?task="+task.ID, nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "没有登记发现") {
		t.Fatalf("failed database became successful report: %d %s", w.Code, w.Body.String())
	}
}
